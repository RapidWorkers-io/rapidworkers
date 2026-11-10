"""TypeScript/JavaScript outputs for scripts/codegen.py.

typescript/rapidworkers/generated.js    tool metadata + method names (runtime)
typescript/rapidworkers/generated.d.ts  parameter interfaces + method signatures
typescript/mcp/tools.json               bundled spec for the stdio proxy
"""
from __future__ import annotations

import json
from pathlib import Path


def camel(name: str) -> str:
    head, *rest = name.split("_")
    return head + "".join(part.capitalize() for part in rest)


def pascal(name: str) -> str:
    return "".join(part.capitalize() for part in name.split("_"))


def ts_type(schema: dict, indent: str = "") -> str:
    if "enum" in schema:
        return " | ".join(json.dumps(v) for v in schema["enum"])
    kind = schema.get("type")
    if kind == "string":
        return "string"
    if kind in ("number", "integer"):
        return "number"
    if kind == "boolean":
        return "boolean"
    if kind == "array":
        inner = ts_type(schema.get("items") or {}, indent)
        return f"ReadonlyArray<{inner}>"
    if kind == "object" and schema.get("properties"):
        required = set(schema.get("required") or [])
        inner_indent = indent + "    "
        lines = ["{"]
        for name, prop in schema["properties"].items():
            desc = prop.get("description")
            if desc:
                lines.append(f"{inner_indent}/** {comment(desc)} */")
            optional = "" if name in required else "?"
            lines.append(f"{inner_indent}{name}{optional}: {ts_type(prop, inner_indent)};")
        lines.append(f"{indent}}}")
        return "\n".join(lines)
    return "Record<string, unknown>"


def comment(text: str) -> str:
    return text.replace("*/", "* /").replace("\n", " ")


def jsdoc(text: str, indent: str, wrap) -> list[str]:
    lines = wrap(comment(text), 96, f"{indent} * ")
    return [f"{indent}/**", *lines, f"{indent} */"]


def gen_js(spec: dict, helpers: dict) -> str:
    tool_meta = helpers["tool_meta"]
    lines = [
        f"// {helpers['HEADER']}",
        "",
        f"export const SERVER_URL = {json.dumps(spec['server']['url'])};",
        "",
        "/** Tool name -> { method, read_only, idempotency_key, retry }. */",
        "export const TOOLS = Object.freeze({",
    ]
    for tool in spec["tools"]:
        meta = {"method": camel(tool["name"]), **tool_meta(tool)}
        lines.append(f"    {tool['name']}: Object.freeze({json.dumps(meta)}),")
    lines += ["});", ""]
    return "\n".join(lines)


def gen_dts(spec: dict, helpers: dict) -> str:
    params, wrap = helpers["params"], helpers["wrap"]
    out = [f"// {helpers['HEADER']}", "", "export declare const SERVER_URL: string;", ""]
    out += [
        "export interface ToolMeta {",
        "    method: string;",
        "    read_only: boolean;",
        "    idempotency_key: boolean;",
        "    retry: boolean;",
        "}",
        "",
        "export declare const TOOLS: Readonly<Record<ToolName, Readonly<ToolMeta>>>;",
        "",
        "export type ToolName =",
    ]
    out += [f"    | {json.dumps(t['name'])}" for t in spec["tools"]]
    out[-1] += ";"
    out.append("")
    for tool in spec["tools"]:
        args = params(tool)
        if not args:
            continue
        out += jsdoc(f"Arguments of {tool['name']}.", "", wrap)
        out.append(f"export interface {pascal(tool['name'])}Params {{")
        for name, schema, required in args:
            desc = schema.get("description", "")
            if name == "idempotency_key":
                desc = "Optional: created automatically when omitted and reused on retries. " + desc
            if desc:
                out += jsdoc(desc, "    ", wrap)
            out.append(f"    {name}{'' if required else '?'}: {ts_type(schema, '    ')};")
        out += ["}", ""]
    out += ["/** One method per RapidWorkers MCP tool. Each resolves to the tool's structured result. */", "export interface ToolMethods {"]
    for tool in spec["tools"]:
        args = params(tool)
        out += jsdoc(tool.get("description", ""), "    ", wrap)
        if not args:
            sig = ""
        elif any(required for _, _, required in args):
            sig = f"params: {pascal(tool['name'])}Params"
        else:
            sig = f"params?: {pascal(tool['name'])}Params"
        out.append(f"    {camel(tool['name'])}({sig}): Promise<Record<string, any>>;")
    out += ["}", ""]
    return "\n".join(out)


def outputs(spec: dict, root: Path, helpers: dict) -> dict:
    sdk = root / "typescript" / "rapidworkers"
    return {
        sdk / "generated.js": lambda: gen_js(spec, helpers),
        sdk / "generated.d.ts": lambda: gen_dts(spec, helpers),
        root / "typescript" / "mcp" / "tools.json": helpers["spec_json"],
    }
