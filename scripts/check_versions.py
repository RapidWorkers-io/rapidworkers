#!/usr/bin/env python3
"""Check that every package and server.json carry the same version.

    python3 scripts/check_versions.py                 # all versions agree
    python3 scripts/check_versions.py --tag npm-v0.1.0  # ...and match a release tag
"""
from __future__ import annotations

import json
import re
import sys
from pathlib import Path

ROOT = Path(__file__).resolve().parent.parent


def python_version(path: str) -> str:
    return re.search(r'__version__\s*=\s*"([^"]+)"', (ROOT / path).read_text()).group(1)


def cargo_version(path: str) -> str | None:
    file = ROOT / path
    if not file.exists():
        return None
    match = re.search(r'^version\s*=\s*"([^"]+)"', file.read_text(), re.M)
    return match.group(1) if match else None


def main() -> int:
    server = json.loads((ROOT / "server.json").read_text())
    found = {
        "server.json": server["version"],
        "python/rapidworkers": python_version("python/rapidworkers/src/rapidworkers/_version.py"),
        "python/rapidworkers-mcp": python_version("python/rapidworkers-mcp/src/rapidworkers_mcp/__init__.py"),
        "typescript/rapidworkers": json.loads((ROOT / "typescript/rapidworkers/package.json").read_text())["version"],
        "typescript/mcp": json.loads((ROOT / "typescript/mcp/package.json").read_text())["version"],
    }
    js_const = re.search(r"VERSION = '([^']+)'", (ROOT / "typescript/rapidworkers/index.js").read_text()).group(1)
    found["typescript/rapidworkers VERSION"] = js_const
    found["typescript/mcp VERSION"] = re.search(r"VERSION = '([^']+)'", (ROOT / "typescript/mcp/server.js").read_text()).group(1)
    go_version = ROOT / "go" / "version.go"
    if go_version.exists():
        found["go"] = re.search(r'Version\s*=\s*"([^"]+)"', go_version.read_text()).group(1)
    for crate in ("rapidworkers", "rapidworkers-mcp"):
        version = cargo_version(f"rust/{crate}/Cargo.toml")
        if version:
            found[f"rust/{crate}"] = version
    for package in server["packages"]:
        version = package.get("version") or package["identifier"].rsplit(":", 1)[-1]
        found[f"server.json {package['registryType']}"] = version

    expected = server["version"]
    tag = next((a.split("=", 1)[1] for a in sys.argv[1:] if a.startswith("--tag=")), None)
    if "--tag" in sys.argv[1:]:
        tag = sys.argv[sys.argv.index("--tag") + 1]
    if tag:
        tag_version = re.sub(r"^(python-|npm-|rust-|docker-|registry-|go/)?v", "", tag)
        found[f"tag {tag}"] = tag_version

    wrong = {k: v for k, v in found.items() if v != expected}
    for key, value in found.items():
        print(f"{'ok ' if value == expected else 'BAD'} {key}: {value}")
    if wrong:
        print(f"\nExpected every version to be {expected}.")
        return 1
    return 0


if __name__ == "__main__":
    sys.exit(main())
