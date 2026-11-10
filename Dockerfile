# RapidWorkers MCP server (stdio). Relays every request to the hosted server at
# https://www.rapidworkers.io/api/mcp/ with RAPIDWORKERS_API_KEY.
#   docker run -i --rm -e RAPIDWORKERS_API_KEY=... ghcr.io/rapidworkers-io/rapidworkers-mcp
FROM node:22-alpine

LABEL io.modelcontextprotocol.server.name="io.rapidworkers/rapidworkers"
LABEL org.opencontainers.image.title="rapidworkers-mcp"
LABEL org.opencontainers.image.description="MCP server for RapidWorkers: let AI agents hire real people for AI data collection, app testing and microtasks."
LABEL org.opencontainers.image.source="https://github.com/RapidWorkers-io/rapidworkers"
LABEL org.opencontainers.image.url="https://www.rapidworkers.io/mcp/"
LABEL org.opencontainers.image.licenses="MIT"

WORKDIR /app
COPY typescript/mcp/package.json typescript/mcp/server.js typescript/mcp/tools.json ./
COPY typescript/mcp/bin ./bin
ENV NODE_ENV=production
USER node
ENTRYPOINT ["node", "bin/rapidworkers-mcp.js"]
