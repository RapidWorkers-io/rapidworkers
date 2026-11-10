#!/usr/bin/env bash
# Builds dist/rapidworkers-mcp-<version>.mcpb (Claude Desktop extension) from
# mcpb/manifest.json + icon and the Node relay in typescript/mcp.
set -euo pipefail
cd "$(dirname "$0")/.."
version="$(node -p "require('./typescript/mcp/package.json').version")"
stage="$(mktemp -d)"
trap 'rm -rf "$stage"' EXIT
mkdir -p "$stage/server" dist
cp mcpb/manifest.json mcpb/icon.png "$stage/"
cp -R typescript/mcp/bin typescript/mcp/server.js typescript/mcp/tools.json typescript/mcp/package.json typescript/mcp/LICENSE "$stage/server/"
npx --yes @anthropic-ai/mcpb@2.1.2 validate "$stage/manifest.json"
npx --yes @anthropic-ai/mcpb@2.1.2 pack "$stage" "dist/rapidworkers-mcp-${version}.mcpb"
echo "dist/rapidworkers-mcp-${version}.mcpb"
