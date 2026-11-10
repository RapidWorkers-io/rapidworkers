#!/usr/bin/env node
import { main } from '../server.js';

main().catch((error) => {
    process.stderr.write(`[rapidworkers-mcp] fatal: ${error?.stack || error}\n`);
    process.exit(1);
});
