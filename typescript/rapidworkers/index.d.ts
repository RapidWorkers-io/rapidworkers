import type { ToolMethods, ToolName } from './generated.js';

export * from './generated.js';

export declare const VERSION: string;
export declare const API_KEY_ENV: 'RAPIDWORKERS_API_KEY';
export declare const BASE_URL_ENV: 'RAPIDWORKERS_MCP_URL';
export declare const API_KEYS_URL: string;

export interface RapidWorkersOptions {
    /** API key; defaults to the RAPIDWORKERS_API_KEY environment variable. */
    apiKey?: string;
    /** MCP endpoint; defaults to RAPIDWORKERS_MCP_URL or https://www.rapidworkers.io/api/mcp/. */
    baseURL?: string;
    /** Per-request timeout in milliseconds (default 60000). */
    timeout?: number;
    /** Retries for rate limits, outages and (for retry-safe calls) network errors (default 2). */
    maxRetries?: number;
    /** Custom fetch implementation (default globalThis.fetch). */
    fetch?: typeof globalThis.fetch;
}

export declare class RapidWorkersError extends Error {}
export declare class ConfigurationError extends RapidWorkersError {}
export declare class APIError extends RapidWorkersError {
    status?: number;
    code?: number;
    data?: unknown;
}
export declare class AuthenticationError extends APIError {}
export declare class RateLimitError extends APIError {
    retryAfter?: number;
}
export declare class ServiceUnavailableError extends APIError {}
export declare class APIConnectionError extends RapidWorkersError {}
export declare class ToolError extends RapidWorkersError {
    tool: string;
    details: Record<string, unknown>;
    code?: string;
}

export declare class RapidWorkers {
    constructor(options?: RapidWorkersOptions);
    readonly apiKey: string;
    readonly baseURL: string;
    readonly timeout: number;
    readonly maxRetries: number;
    /** Send one JSON-RPC request (e.g. "tools/list") and return its result. */
    request(method: string, params?: Record<string, unknown>): Promise<any>;
    /** Call any tool by name. Rejects with ToolError when the tool refuses the call. */
    callTool(name: ToolName | (string & {}), args?: Record<string, unknown>): Promise<Record<string, any>>;
    /** The live tool definitions (name, description, inputSchema, annotations). */
    listTools(): Promise<Array<Record<string, any>>>;
}

export interface RapidWorkers extends ToolMethods {}

export default RapidWorkers;
