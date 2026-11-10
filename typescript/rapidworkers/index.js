// Official JavaScript/TypeScript client for RapidWorkers.
//
// Every tool call is one JSON-RPC `tools/call` POST to the hosted, stateless
// RapidWorkers MCP server, so the SDK gets exactly the validation, cost quotes,
// safeguards and error messages the server gives AI agents.
import { randomUUID } from 'node:crypto';

import { SERVER_URL, TOOLS } from './generated.js';

export { SERVER_URL, TOOLS };

export const VERSION = '0.1.0';
export const API_KEY_ENV = 'RAPIDWORKERS_API_KEY';
export const BASE_URL_ENV = 'RAPIDWORKERS_MCP_URL';
export const API_KEYS_URL = 'https://www.rapidworkers.io/dashboard/api-management/';
const PROTOCOL_VERSION = '2025-06-18';
// Answered before any tool ran: safe to resend for every call
const RETRY_ALWAYS = new Set([429, 503]);
// A gateway error may come after the tool ran: resend only retry-safe calls
const RETRY_IF_SAFE = new Set([502, 504]);

export class RapidWorkersError extends Error {
    constructor(message, options) {
        super(message, options);
        this.name = new.target.name;
    }
}

/** The client is missing something it needs, such as an API key. */
export class ConfigurationError extends RapidWorkersError {}

/** The server answered with an HTTP or JSON-RPC error. */
export class APIError extends RapidWorkersError {
    constructor(message, { status, code, data } = {}) {
        super(message);
        this.status = status;
        this.code = code;
        this.data = data;
    }
}

/** The API key is missing, invalid, revoked, or the account cannot use it (HTTP 401). */
export class AuthenticationError extends APIError {}

/** Too many requests for this API key (HTTP 429). */
export class RateLimitError extends APIError {
    constructor(message, { retryAfter, ...rest } = {}) {
        super(message, rest);
        this.retryAfter = retryAfter;
    }
}

/** RapidWorkers is temporarily unavailable (HTTP 502, 503 or 504). */
export class ServiceUnavailableError extends APIError {}

/** The request did not reach RapidWorkers or timed out with no answer. */
export class APIConnectionError extends RapidWorkersError {}

/** The tool ran and refused the call; `message` says how to fix it. */
export class ToolError extends RapidWorkersError {
    constructor(tool, message, details = {}) {
        super(message);
        this.tool = tool;
        this.details = details;
        this.code = typeof details.code === 'string' ? details.code : undefined;
    }
}

const sleep = (ms) => new Promise((resolve) => setTimeout(resolve, ms));

function retryAfterSeconds(response) {
    const value = Number(response?.headers.get('retry-after'));
    return Number.isFinite(value) && value >= 0 ? value : undefined;
}

function backoffMs(attempt, response) {
    const hinted = retryAfterSeconds(response);
    return Math.min(hinted ?? 0.5 * 2 ** attempt, 30) * 1000;
}

function prepare(name, args) {
    const clean = Object.fromEntries(Object.entries(args || {}).filter(([, value]) => value !== undefined && value !== null));
    if (TOOLS[name]?.idempotency_key && !clean.idempotency_key) {
        clean.idempotency_key = `sdk-${randomUUID().replaceAll('-', '')}`;
    }
    return clean;
}

function rpcMessage(text, contentType, id) {
    if (contentType.includes('text/event-stream')) {
        for (const line of text.split(/\r?\n/)) {
            if (!line.startsWith('data:')) continue;
            try {
                const message = JSON.parse(line.slice(5).trim());
                if (message && message.id === id) return message;
            } catch {
                // not JSON: skip
            }
        }
        return null;
    }
    try {
        const message = JSON.parse(text);
        if (Array.isArray(message)) return message.find((m) => m && m.id === id) || null;
        return message && typeof message === 'object' ? message : null;
    } catch {
        return null;
    }
}

function httpError(response, message, text) {
    const error = message?.error || {};
    const msg = error.message || text.slice(0, 500) || `HTTP ${response.status}`;
    const info = { status: response.status, code: error.code, data: error.data };
    if (response.status === 401) return new AuthenticationError(`${msg} Check your API key or create one at ${API_KEYS_URL}`, info);
    if (response.status === 429) return new RateLimitError(msg, { ...info, retryAfter: retryAfterSeconds(response) });
    if ([502, 503, 504].includes(response.status)) return new ServiceUnavailableError(msg, info);
    return new APIError(msg, info);
}

function toolResult(tool, result) {
    const structured = result?.structuredContent;
    const text = (result?.content || [])
        .filter((block) => block && block.type === 'text')
        .map((block) => block.text || '')
        .join('\n');
    if (result?.isError) {
        const { message, ...details } = structured?.error && typeof structured.error === 'object' ? structured.error : {};
        throw new ToolError(tool, message || text || 'The tool call failed.', details);
    }
    if (structured && typeof structured === 'object' && !Array.isArray(structured)) return structured;
    // Text-only answers carry a one-line summary, a blank line and the JSON
    const split = text.indexOf('\n\n');
    try {
        const data = JSON.parse(split >= 0 ? text.slice(split + 2) : text);
        return data && typeof data === 'object' && !Array.isArray(data) ? data : { data };
    } catch {
        return { text };
    }
}

/**
 * RapidWorkers client.
 *
 *   import { RapidWorkers } from 'rapidworkers';
 *   const rw = new RapidWorkers(); // reads RAPIDWORKERS_API_KEY
 *   console.log(await rw.getAccount());
 *
 * Every tool of the RapidWorkers MCP server is a method. Calls that move money
 * get an idempotency key automatically, and a timed-out call is retried with
 * the same key, so it never happens twice.
 */
export class RapidWorkers {
    #ids = 0;

    constructor({ apiKey, baseURL, timeout = 60_000, maxRetries = 2, fetch: fetchImpl } = {}) {
        const key = (apiKey ?? globalThis.process?.env?.[API_KEY_ENV] ?? '').trim();
        if (!key) {
            throw new ConfigurationError(
                `No RapidWorkers API key. Pass { apiKey } or set ${API_KEY_ENV}. Create a key at ${API_KEYS_URL}`
            );
        }
        this.apiKey = key;
        this.baseURL = baseURL || globalThis.process?.env?.[BASE_URL_ENV] || SERVER_URL;
        this.timeout = timeout;
        this.maxRetries = Math.max(0, maxRetries);
        this.fetch = fetchImpl || globalThis.fetch;
        if (typeof this.fetch !== 'function') throw new ConfigurationError('fetch is not available; use Node 20+ or pass { fetch }.');
    }

    /** Send one JSON-RPC request (e.g. "tools/list") and return its result. */
    async request(method, params = {}) {
        const retrySafe = method !== 'tools/call' || Boolean(TOOLS[params.name]?.retry);
        for (let attempt = 0; ; attempt += 1) {
            const id = ++this.#ids;
            let response;
            try {
                response = await this.fetch(this.baseURL, {
                    method: 'POST',
                    headers: {
                        'X-API-Key': this.apiKey,
                        'Content-Type': 'application/json',
                        Accept: 'application/json, text/event-stream',
                        'MCP-Protocol-Version': PROTOCOL_VERSION,
                        'User-Agent': `rapidworkers-js/${VERSION}`,
                    },
                    body: JSON.stringify({ jsonrpc: '2.0', id, method, params }),
                    signal: AbortSignal.timeout(this.timeout),
                });
            } catch (error) {
                if (retrySafe && attempt < this.maxRetries) {
                    await sleep(backoffMs(attempt));
                    continue;
                }
                throw new APIConnectionError(`Could not reach RapidWorkers: ${error?.message || error}`, { cause: error });
            }
            const retry = RETRY_ALWAYS.has(response.status) || (retrySafe && RETRY_IF_SAFE.has(response.status));
            if (retry && attempt < this.maxRetries) {
                await response.body?.cancel().catch(() => {});
                await sleep(backoffMs(attempt, response));
                continue;
            }
            const text = await response.text();
            const message = rpcMessage(text, response.headers.get('content-type') || '', id);
            if (response.status >= 400) throw httpError(response, message, text);
            if (!message) throw new APIError('Unreadable answer from RapidWorkers.', { status: response.status });
            if (message.error) {
                throw new APIError(message.error.message || 'JSON-RPC error', {
                    status: response.status,
                    code: message.error.code,
                    data: message.error.data,
                });
            }
            return method === 'tools/call' ? toolResult(params.name, message.result) : message.result;
        }
    }

    /** Call any tool by name. Throws ToolError when the tool refuses the call. */
    callTool(name, args = {}) {
        return this.request('tools/call', { name, arguments: prepare(name, args) });
    }

    /** The live tool definitions (name, description, inputSchema, annotations). */
    async listTools() {
        const result = await this.request('tools/list');
        return result?.tools || [];
    }
}

for (const [name, meta] of Object.entries(TOOLS)) {
    Object.defineProperty(RapidWorkers.prototype, meta.method, {
        value(args) {
            return this.callTool(name, args);
        },
        writable: true,
        configurable: true,
    });
}

export default RapidWorkers;
