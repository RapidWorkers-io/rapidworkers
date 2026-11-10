"""MCPB (Claude Desktop extension) manifest for scripts/codegen.py.

mcpb/manifest.json: bundles the Node stdio relay from typescript/mcp; the
bundle itself is assembled by scripts/build_mcpb.sh.
"""
from __future__ import annotations

import json
import re
from pathlib import Path


def first_sentence(text: str) -> str:
    match = re.match(r"(.+?[.!?])(\s|$)", text.strip())
    return (match.group(1) if match else text.strip())[:200]


def manifest(spec: dict, root: Path) -> str:
    version = json.loads((root / "typescript" / "mcp" / "package.json").read_text())["version"]
    data = {
        "$schema": "https://raw.githubusercontent.com/modelcontextprotocol/mcpb/main/dist/mcpb-manifest-v0.3.schema.json",
        "manifest_version": "0.3",
        "name": "rapidworkers",
        "display_name": "RapidWorkers",
        "version": version,
        "description": "Hire real people for AI data collection (photos, audio, video), app testing and microtasks.",
        "long_description": (
            "RapidWorkers connects your AI assistant to people who do paid microtasks on their own devices: "
            "AI training data collection (photos, audio, video, text), real-device app and website testing, "
            "surveys and native-speaker checks. The assistant gets a cost quote, asks you to confirm it, posts "
            "the task, then reviews each submission against your brief. Requires a RapidWorkers API key."
        ),
        "author": {"name": "RapidWorkers", "url": "https://www.rapidworkers.io/"},
        "repository": {"type": "git", "url": "https://github.com/RapidWorkers-io/rapidworkers"},
        "homepage": "https://www.rapidworkers.io/mcp/",
        "documentation": "https://www.rapidworkers.io/developers/#mcp",
        "icon": "icon.png",
        "server": {
            "type": "node",
            "entry_point": "server/bin/rapidworkers-mcp.js",
            "mcp_config": {
                "command": "node",
                "args": ["${__dirname}/server/bin/rapidworkers-mcp.js"],
                "env": {"RAPIDWORKERS_API_KEY": "${user_config.api_key}"},
            },
        },
        "tools": [{"name": t["name"], "description": first_sentence(t.get("description", ""))} for t in spec["tools"]],
        "keywords": ["crowdsourcing", "human-in-the-loop", "data-collection", "app-testing", "microtasks"],
        "license": "MIT",
        "compatibility": {"platforms": ["darwin", "win32", "linux"], "runtimes": {"node": ">=20.0.0"}},
        "user_config": {
            "api_key": {
                "type": "string",
                "title": "RapidWorkers API key",
                "description": "Create one at https://www.rapidworkers.io/dashboard/api-management/",
                "sensitive": True,
                "required": True,
            }
        },
    }
    return json.dumps(data, indent=2) + "\n"


def outputs(spec: dict, root: Path, helpers: dict) -> dict:
    return {root / "mcpb" / "manifest.json": lambda: manifest(spec, root)}
