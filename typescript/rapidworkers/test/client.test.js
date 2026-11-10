import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import { test } from 'node:test';

import {
    AuthenticationError,
    ConfigurationError,
    RapidWorkers,
    ServiceUnavailableError,
    TOOLS,
    ToolError,
} from '../index.js';

const ENDPOINT = 'https://mcp.test/api/mcp/';
const KEY = 'k'.repeat(64);

function structured(data) {
    return { content: [{ type: 'text', text: `summary\n\n${JSON.stringify(data)}` }], structuredContent: data };
}

/** fetch stub: each entry is a result object, a Response factory, or an Error. Records every call. */
function stub(...answers) {
    const calls = [];
    const fetch = async (url, init) => {
        const body = JSON.parse(init.body);
        calls.push({ url, init, body });
        const answer = answers.length > 1 ? answers.shift() : answers[0];
        if (answer instanceof Error) throw answer;
        if (typeof answer === 'function') return answer(body);
        return Response.json({ jsonrpc: '2.0', id: body.id, result: answer });
    };
    return { fetch, calls };
}

const client = (fetch) => new RapidWorkers({ apiKey: KEY, baseURL: ENDPOINT, fetch });
const noWait = { retryAfter: () => new Response(null, { status: 429, headers: { 'retry-after': '0' } }) };

test('requires an API key', () => {
    const saved = process.env.RAPIDWORKERS_API_KEY;
    delete process.env.RAPIDWORKERS_API_KEY;
    try {
        assert.throws(() => new RapidWorkers(), (e) => e instanceof ConfigurationError && /api-management/.test(e.message));
    } finally {
        if (saved !== undefined) process.env.RAPIDWORKERS_API_KEY = saved;
    }
});

test('sends JSON-RPC tools/call with the API key and returns structuredContent', async () => {
    const { fetch, calls } = stub(structured({ balance: '12.50' }));
    assert.deepEqual(await client(fetch).getAccount(), { balance: '12.50' });
    const [{ url, init, body }] = calls;
    assert.equal(url, ENDPOINT);
    assert.equal(body.method, 'tools/call');
    assert.deepEqual(body.params, { name: 'get_account', arguments: {} });
    assert.equal(init.headers['X-API-Key'], KEY);
    assert.match(init.headers.Accept, /text\/event-stream/);
});

test('drops undefined and null arguments', async () => {
    const { fetch, calls } = stub(structured({ jobs: [] }));
    await client(fetch).listJobs({ kind: 'ai_task', status: undefined, search: null });
    assert.deepEqual(calls[0].body.params.arguments, { kind: 'ai_task' });
});

test('money calls get one idempotency key, reused on retries', async () => {
    const { fetch, calls } = stub(new TypeError('fetch failed'), () => new Response('', { status: 504 }), structured({ ok: true }));
    const rw = client(fetch);
    await rw.approveSubmission({ job_id: 'x', submission_id: 5 });
    const keys = new Set(calls.map((c) => c.body.params.arguments.idempotency_key));
    assert.equal(calls.length, 3);
    assert.equal(keys.size, 1);
    assert.match([...keys][0], /^sdk-[0-9a-f]{32}$/);
});

test('keeps an explicit idempotency key', async () => {
    const { fetch, calls } = stub(structured({ ok: true }));
    await client(fetch).cancelJob({ job_id: 'x', confirm: true, idempotency_key: 'mine-1' });
    assert.equal(calls[0].body.params.arguments.idempotency_key, 'mine-1');
});

test('a write without an idempotency key is not retried after a gateway error', async () => {
    const { fetch, calls } = stub(() => new Response('gateway timeout', { status: 504 }));
    await assert.rejects(client(fetch).getDepositLink({ amount: 20 }), ServiceUnavailableError);
    assert.equal(calls.length, 1);
});

test('429 is retried for any tool', async () => {
    const { fetch, calls } = stub(noWait.retryAfter, structured({ url: 'https://pay' }));
    assert.deepEqual(await client(fetch).getDepositLink({ amount: 20 }), { url: 'https://pay' });
    assert.equal(calls.length, 2);
});

test('a refused tool call throws ToolError with details', async () => {
    const { fetch } = stub({
        content: [{ type: 'text', text: 'Balance too low.' }],
        structuredContent: { error: { message: 'Balance too low.', code: 'insufficient_balance' } },
        isError: true,
    });
    await assert.rejects(client(fetch).previewJobCost({ pay_rate: 0.05, quantity: 10 }), (e) => {
        assert.ok(e instanceof ToolError);
        assert.equal(e.message, 'Balance too low.');
        assert.equal(e.code, 'insufficient_balance');
        assert.equal(e.tool, 'preview_job_cost');
        return true;
    });
});

test('401 throws AuthenticationError', async () => {
    const { fetch } = stub(() =>
        Response.json({ jsonrpc: '2.0', id: null, error: { code: -32001, message: 'Invalid token.' } }, { status: 401 })
    );
    await assert.rejects(client(fetch).getAccount(), (e) => e instanceof AuthenticationError && /Invalid token/.test(e.message));
});

test('parses an SSE answer', async () => {
    const { fetch } = stub((body) => {
        const message = JSON.stringify({ jsonrpc: '2.0', id: body.id, result: structured({ n: 1 }) });
        return new Response(`event: message\ndata: ${message}\n\n`, { headers: { 'content-type': 'text/event-stream' } });
    });
    assert.deepEqual(await client(fetch).listBlockedWorkers(), { n: 1 });
});

test('parses a text-only result', async () => {
    const { fetch } = stub({ content: [{ type: 'text', text: 'One job.\n\n{"jobs":[1]}' }] });
    assert.deepEqual(await client(fetch).listJobs(), { jobs: [1] });
});

test('listTools returns the live definitions', async () => {
    const { fetch } = stub({ tools: [{ name: 'get_account' }] });
    assert.deepEqual(await client(fetch).listTools(), [{ name: 'get_account' }]);
});

test('every spec tool has a method', () => {
    const spec = JSON.parse(readFileSync(new URL('../../../spec/tools.json', import.meta.url)));
    assert.equal(spec.tools.length, Object.keys(TOOLS).length);
    for (const tool of spec.tools) {
        assert.equal(typeof RapidWorkers.prototype[TOOLS[tool.name].method], 'function', tool.name);
    }
});
