import assert from 'node:assert/strict';
import { spawnSync } from 'node:child_process';
import { test } from 'node:test';
import { fileURLToPath } from 'node:url';

import { Proxy, loadSpec } from '../server.js';

const ENDPOINT = 'https://mcp.test/api/mcp/';
const KEY = 'k'.repeat(64);
const SPEC = loadSpec();
const rpc = (method, id = 1, params = {}) => ({ jsonrpc: '2.0', id, method, params });

function stub(...answers) {
    const calls = [];
    const fetch = async (url, init) => {
        calls.push({ url, init, body: JSON.parse(init.body) });
        const answer = answers.length > 1 ? answers.shift() : answers[0];
        if (answer instanceof Error) throw answer;
        return answer(JSON.parse(init.body));
    };
    return { fetch, calls };
}

const proxy = (fetch) => new Proxy({ apiKey: KEY, url: ENDPOINT, spec: SPEC, fetch, delay: async () => {} });
const ok = (result) => (body) => Response.json({ jsonrpc: '2.0', id: body.id, result });

test('forwards the request unchanged with the API key', async () => {
    const { fetch, calls } = stub(ok({ ok: 1 }));
    const message = rpc('tools/call', 7, { name: 'get_account', arguments: {} });
    assert.deepEqual(await proxy(fetch).handle(message), { jsonrpc: '2.0', id: 7, result: { ok: 1 } });
    assert.deepEqual(calls[0].body, message);
    assert.equal(calls[0].init.headers['X-API-Key'], KEY);
});

test('initialize sets the protocol header for later calls', async () => {
    const { fetch, calls } = stub(ok({ protocolVersion: '2025-06-18' }));
    const p = proxy(fetch);
    await p.handle(rpc('initialize', 1, { protocolVersion: '2025-06-18' }));
    await p.handle(rpc('tools/list', 2));
    assert.equal(calls[1].init.headers['MCP-Protocol-Version'], '2025-06-18');
});

test('parses an SSE answer', async () => {
    const { fetch } = stub((body) => {
        const data = JSON.stringify({ jsonrpc: '2.0', id: body.id, result: { tools: [] } });
        return new Response(`event: message\ndata: ${data}\n\n`, { headers: { 'content-type': 'text/event-stream' } });
    });
    assert.deepEqual((await proxy(fetch).handle(rpc('tools/list', 'abc'))).result, { tools: [] });
});

test('401 becomes a JSON-RPC error with a key hint', async () => {
    const { fetch } = stub(() =>
        Response.json({ jsonrpc: '2.0', id: null, error: { code: -32001, message: 'Invalid token.' } }, { status: 401 })
    );
    const answer = await proxy(fetch).handle(rpc('tools/list', 3));
    assert.equal(answer.id, 3);
    assert.equal(answer.error.code, -32001);
    assert.match(answer.error.message, /api-management/);
});

test('503 is retried', async () => {
    const { fetch, calls } = stub(() => new Response('', { status: 503 }), ok({}));
    assert.deepEqual((await proxy(fetch).handle(rpc('tools/list', 4))).result, {});
    assert.equal(calls.length, 2);
});

test('a network error on a tool call mentions the idempotency key', async () => {
    const { fetch } = stub(new TypeError('fetch failed'));
    const answer = await proxy(fetch).handle(rpc('tools/call', 5, { name: 'approve_submission', arguments: {} }));
    assert.match(answer.error.message, /same idempotency_key/);
});

test('notifications get no answer and ping is answered locally', async () => {
    const { fetch, calls } = stub(ok({}));
    const p = proxy(fetch);
    assert.equal(await p.handle({ jsonrpc: '2.0', method: 'notifications/initialized' }), null);
    assert.deepEqual(await p.handle(rpc('ping', 9)), { jsonrpc: '2.0', id: 9, result: {} });
    assert.equal(calls.length, 0);
});

test('over real stdio without a key: lists tools and explains calls', () => {
    const lines = [
        rpc('initialize', 1, { protocolVersion: '2025-06-18', capabilities: {}, clientInfo: { name: 't', version: '1' } }),
        { jsonrpc: '2.0', method: 'notifications/initialized' },
        rpc('tools/list', 2),
        rpc('tools/call', 3, { name: 'get_account', arguments: {} }),
    ];
    const env = { ...process.env };
    delete env.RAPIDWORKERS_API_KEY;
    const bin = fileURLToPath(new URL('../bin/rapidworkers-mcp.js', import.meta.url));
    const done = spawnSync(process.execPath, [bin], {
        input: `${lines.map((m) => JSON.stringify(m)).join('\n')}\n`,
        env,
        encoding: 'utf8',
        timeout: 30_000,
    });
    const answers = Object.fromEntries(done.stdout.trim().split('\n').map((l) => JSON.parse(l)).map((a) => [a.id, a]));
    assert.deepEqual(Object.keys(answers).sort(), ['1', '2', '3']);
    assert.equal(answers[1].result.serverInfo.name, 'rapidworkers');
    assert.equal(answers[1].result.protocolVersion, '2025-06-18');
    assert.equal(answers[2].result.tools.length, SPEC.tools.length);
    assert.equal(SPEC.tools.length, 20);
    assert.equal(answers[3].result.isError, true);
    assert.match(answers[3].result.content[0].text, /RAPIDWORKERS_API_KEY/);
    assert.match(done.stderr, /not set/);
});
