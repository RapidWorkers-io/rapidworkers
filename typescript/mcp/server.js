// stdio MCP server that relays every request to the hosted RapidWorkers MCP
// server (https://www.rapidworkers.io/api/mcp/) with the API key, so the
// tools, instructions, prompts and resources are always exactly the live ones.
//
// Without RAPIDWORKERS_API_KEY it still starts and lists the tools from a
// bundled snapshot (so registries and inspectors can see them); every tool
// call then explains how to create a key.
import { readFileSync } from 'node:fs';
import { createInterface } from 'node:readline';

export const VERSION = '0.1.0';
export const API_KEY_ENV = 'RAPIDWORKERS_API_KEY';
export const URL_ENV = 'RAPIDWORKERS_MCP_URL';
export const API_KEYS_URL = 'https://www.rapidworkers.io/dashboard/api-management/';
const KNOWN_PROTOCOL_VERSIONS = ['2025-11-25', '2025-06-18', '2025-03-26', '2024-11-05'];
const DEFAULT_PROTOCOL_VERSION = '2025-06-18';
// Answered before anything ran upstream, so resending is always safe
const RETRY_STATUS = new Set([429, 503]);
const MAX_RETRIES = 2;
const REQUEST_TIMEOUT_MS = 120_000;

const PARSE_ERROR = -32700;
const INVALID_REQUEST = -32600;
const METHOD_NOT_FOUND = -32601;
const INTERNAL_ERROR = -32603;
const UPSTREAM_ERROR = -32000;

export const NO_KEY_MESSAGE =
    `${API_KEY_ENV} is not set. Create an API key at ${API_KEYS_URL} and add it to this MCP server's ` +
    `environment (for example "env": {"${API_KEY_ENV}": "..."}), then restart the client.`;

export function loadSpec() {
    return JSON.parse(readFileSync(new URL('./tools.json', import.meta.url), 'utf8'));
}

const log = (message) => process.stderr.write(`[rapidworkers-mcp] ${message}\n`);
const errorResponse = (id, code, message, data) => ({
    jsonrpc: '2.0',
    id,
    error: data === undefined ? { code, message } : { code, message, data },
});
const resultResponse = (id, result) => ({ jsonrpc: '2.0', id, result });
const sleep = (ms) => new Promise((resolve) => setTimeout(resolve, ms));

function parseUpstream(text, contentType, id) {
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

function retryDelayMs(response, attempt) {
    const hinted = Number(response.headers.get('retry-after'));
    const seconds = Number.isFinite(hinted) && response.headers.has('retry-after') ? hinted : 0.5 * 2 ** attempt;
    return Math.min(Math.max(seconds, 0), 30) * 1000;
}

export class Proxy {
    constructor({ apiKey, url, spec, fetch: fetchImpl = globalThis.fetch, delay = sleep }) {
        this.apiKey = apiKey;
        this.url = url;
        this.spec = spec;
        this.fetch = fetchImpl;
        this.delay = delay;
        this.protocolVersion = null;
    }

    /** The answer to one client message, or null for notifications. */
    async handle(message) {
        if (!message || typeof message !== 'object' || Array.isArray(message) || message.jsonrpc !== '2.0') {
            return errorResponse(null, INVALID_REQUEST, 'Invalid JSON-RPC message.');
        }
        if (typeof message.method !== 'string') return null; // a response to a server request; this server sends none
        if (!('id' in message)) return null; // notifications (initialized, cancelled...) need no answer
        if (message.method === 'ping') return resultResponse(message.id, {});
        if (!this.apiKey) return this.offline(message.id, message.method, message.params || {});
        return this.forward(message);
    }

    async forward(message) {
        const { id } = message;
        const headers = {
            'X-API-Key': this.apiKey,
            'Content-Type': 'application/json',
            Accept: 'application/json, text/event-stream',
            'User-Agent': `rapidworkers-mcp-node/${VERSION}`,
        };
        if (this.protocolVersion) headers['MCP-Protocol-Version'] = this.protocolVersion;
        let response;
        for (let attempt = 0; ; attempt += 1) {
            try {
                response = await this.fetch(this.url, {
                    method: 'POST',
                    headers,
                    body: JSON.stringify(message),
                    signal: AbortSignal.timeout(REQUEST_TIMEOUT_MS),
                });
            } catch (error) {
                const note = message.method === 'tools/call' ? ' If this call moves money, retry it with the same idempotency_key.' : '';
                return errorResponse(id, UPSTREAM_ERROR, `Could not reach RapidWorkers (${error?.name || 'Error'}).${note}`);
            }
            if (RETRY_STATUS.has(response.status) && attempt < MAX_RETRIES) {
                await response.body?.cancel().catch(() => {});
                await this.delay(retryDelayMs(response, attempt + 1));
                continue;
            }
            break;
        }
        const text = await response.text();
        const answer = parseUpstream(text, response.headers.get('content-type') || '', id);
        if (response.status >= 400 || !answer) {
            const upstream = answer?.error || {};
            let msg = upstream.message || `RapidWorkers answered HTTP ${response.status}.`;
            if (response.status === 401) msg = `${msg} Check ${API_KEY_ENV} or create a new key at ${API_KEYS_URL}`;
            return errorResponse(id, upstream.code || UPSTREAM_ERROR, msg, upstream.data);
        }
        if (message.method === 'initialize' && answer.result && typeof answer.result === 'object') {
            const version = answer.result.protocolVersion;
            this.protocolVersion = typeof version === 'string' ? version : null;
        }
        return { ...answer, id };
    }

    /** Answers without an API key: enough to start and list tools. */
    offline(id, method, params) {
        const { server } = this.spec;
        switch (method) {
            case 'initialize': {
                const requested = params.protocolVersion;
                return resultResponse(id, {
                    protocolVersion: KNOWN_PROTOCOL_VERSIONS.includes(requested) ? requested : DEFAULT_PROTOCOL_VERSION,
                    capabilities: { tools: { listChanged: false } },
                    serverInfo: { name: server.name, title: server.title, version: VERSION },
                    instructions: `${server.instructions}\n\n${NO_KEY_MESSAGE}`,
                });
            }
            case 'tools/list':
                return resultResponse(id, { tools: this.spec.tools });
            case 'tools/call':
                return resultResponse(id, { content: [{ type: 'text', text: NO_KEY_MESSAGE }], isError: true });
            case 'resources/list':
                return resultResponse(id, { resources: [] });
            case 'resources/templates/list':
                return resultResponse(id, { resourceTemplates: [] });
            case 'prompts/list':
                return resultResponse(id, { prompts: [] });
            default:
                return errorResponse(id, METHOD_NOT_FOUND, `${method} needs ${API_KEY_ENV}. ${NO_KEY_MESSAGE}`);
        }
    }
}

const write = (message) => process.stdout.write(`${JSON.stringify(message)}\n`);

async function answer(proxy, line) {
    let message;
    try {
        message = JSON.parse(line);
    } catch {
        write(errorResponse(null, PARSE_ERROR, 'Parse error.'));
        return;
    }
    for (const item of Array.isArray(message) ? message : [message]) {
        let reply;
        try {
            reply = await proxy.handle(item);
        } catch (error) {
            // never let one request kill the server
            log(`internal error: ${error?.stack || error}`);
            reply = errorResponse(item && typeof item === 'object' ? (item.id ?? null) : null, INTERNAL_ERROR, 'Internal error in rapidworkers-mcp.');
        }
        if (reply) write(reply);
    }
}

export async function main(argv = process.argv.slice(2)) {
    if (argv.includes('-V') || argv.includes('--version')) {
        process.stdout.write(`rapidworkers-mcp ${VERSION}\n`);
        return;
    }
    if (argv.includes('-h') || argv.includes('--help')) {
        process.stdout.write(
            'rapidworkers-mcp: stdio MCP server for RapidWorkers (relays to https://www.rapidworkers.io/api/mcp/).\n' +
                `Environment: ${API_KEY_ENV} (required for tool calls), ${URL_ENV} (optional endpoint override).\n`
        );
        return;
    }
    const spec = loadSpec();
    const apiKey = (process.env[API_KEY_ENV] || '').trim();
    const url = (process.env[URL_ENV] || '').trim() || spec.server.url;
    if (!apiKey) log(`${API_KEY_ENV} is not set: tools are listed but calls will ask for a key (${API_KEYS_URL}).`);
    const proxy = new Proxy({ apiKey, url, spec });
    const pending = new Set();
    const lines = createInterface({ input: process.stdin, crlfDelay: Infinity });
    for await (const line of lines) {
        if (!line.trim()) continue;
        const task = answer(proxy, line).finally(() => pending.delete(task));
        pending.add(task);
    }
    await Promise.allSettled(pending);
}
