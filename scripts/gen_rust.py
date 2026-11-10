"""Rust outputs for scripts/codegen.py (the crates in rust/).

Writes the typed tool layer of the `rapidworkers` crate (one arguments struct and
one async method per tool), a test per tool that checks the method sends that
tool's name and arguments, and the tools.json snapshot bundled into the
`rapidworkers-mcp` binary.
"""
from __future__ import annotations

import json
import re
from pathlib import Path

LINE_WIDTH = 100
# rustfmt's default widths, so the generated code reads like formatted code
CALL_WIDTH = 60
CHAIN_WIDTH = 60
STRUCT_LIT_WIDTH = 18

RUST_KEYWORDS = {
    "abstract", "as", "async", "await", "become", "box", "break", "const", "continue", "crate", "do",
    "dyn", "else", "enum", "extern", "false", "final", "fn", "for", "gen", "if", "impl", "in", "let",
    "loop", "macro", "match", "mod", "move", "mut", "override", "priv", "pub", "ref", "return", "self",
    "Self", "static", "struct", "super", "trait", "true", "try", "type", "typeof", "unsafe", "unsized",
    "use", "virtual", "where", "while", "yield",
}
# Keywords that cannot be raw identifiers get a trailing underscore instead
NOT_RAW = {"crate", "self", "Self", "super", "_"}
# Hand-written Client methods (rust/rapidworkers/src/client.rs) a tool must not shadow
CLIENT_METHODS = {
    "new", "from_env", "builder", "base_url", "timeout", "max_retries", "request", "call_tool",
    "list_tools",
}

# Names for string enums: by (tool or struct, property), or by property when
# every use of that property has the same values. Anything else gets
# PascalCase(property), or Owner + Property when the values differ per tool.
ENUM_NAMES = {
    "kind": "JobKind",
    "section": "PlatformInfoSection",
    "method": "DepositMethod",
    "task_type": "AiTaskType",
    "capture_mode": "CaptureMode",
    ("create_prefilled_job", "type"): "PrefilledJobType",
    ("list_jobs", "status"): "JobStatus",
    ("list_submissions", "status"): "SubmissionStatus",
    ("SubmissionField", "type"): "SubmissionFieldType",
}
# Names for nested object schemas, by (owner, property)
STRUCT_NAMES = {
    ("create_ai_task", "submission_fields"): "SubmissionField",
}


def fail(message: str) -> None:
    raise SystemExit(f"gen_rust: {message}")


# --------------------------------------------------------------------------- names and text


def pascal(name: str) -> str:
    parts = re.split(r"[^0-9A-Za-z]+|(?<=[a-z0-9])(?=[A-Z])", name)
    out = "".join(p[:1].upper() + (p[1:].lower() if p.isupper() else p[1:]) for p in parts if p)
    if not out or out[0].isdigit():
        out = "V" + out
    return out


def snake(name: str) -> str:
    ident = re.sub(r"(?<=[a-z0-9])(?=[A-Z])", "_", name)
    ident = re.sub(r"[^0-9A-Za-z_]", "_", ident).lower()
    if not ident or ident[0].isdigit():
        ident = "_" + ident
    if ident in NOT_RAW:
        return ident + "_"
    if ident in RUST_KEYWORDS:
        return "r#" + ident
    return ident


def rust_str(text: str) -> str:
    out = ['"']
    for ch in text:
        if ch == '"':
            out.append('\\"')
        elif ch == "\\":
            out.append("\\\\")
        elif ch == "\n":
            out.append("\\n")
        elif ch == "\r":
            out.append("\\r")
        elif ch == "\t":
            out.append("\\t")
        elif ord(ch) < 0x20 or ord(ch) == 0x7F:
            out.append(f"\\u{{{ord(ch):x}}}")
        else:
            out.append(ch)
    out.append('"')
    return "".join(out)


# A word that would start a Markdown list, quote, heading or table when it
# begins a doc line; such words stay on the previous line.
MARKDOWN_START = re.compile(r"^([-+*>#|=]|\d+[.)]$|```)")


def doc(text: str, indent: str) -> list[str]:
    prefix = indent + "/// "
    out: list[str] = []
    for paragraph in text.split("\n"):
        words = paragraph.split()
        if not words:
            out.append(indent + "///")
            continue
        line = words[0]
        for word in words[1:]:
            if len(prefix) + len(line) + 1 + len(word) <= LINE_WIDTH or MARKDOWN_START.match(word):
                line += " " + word
            else:
                out.append(prefix + line)
                line = word
        out.append(prefix + line)
    return out


def call(head: str, args: list[str], indent: str, tail: str = "") -> list[str]:
    """`head(args)tail` on one line when rustfmt would keep it there, else one argument per line."""
    flat = ", ".join(args)
    one_line = f"{indent}{head}({flat}){tail}"
    if "\n" not in flat and len(flat) <= CALL_WIDTH and len(one_line) <= LINE_WIDTH:
        return [one_line]
    inner = indent + "    "
    lines = [f"{indent}{head}("]
    for arg in args:
        lines.append(f"{inner}{arg},")
    lines.append(f"{indent}){tail}")
    return lines


# --------------------------------------------------------------------------- model


class Field:
    def __init__(self, gen: "RustGen", owner: str, name: str, schema: dict, required: bool):
        self.name = name
        self.schema = schema
        self.required = required
        self.ident = snake(name)
        self.ty = gen.rust_type(schema, owner, name)

    @property
    def rust(self) -> str:
        return self.ty.rust if self.required else f"Option<{self.ty.rust}>"


class Type:
    def __init__(self, rust: str, arg: str | None = None, convert: str = "{v}", sample=None):
        self.rust = rust
        self.arg = arg or rust
        self.convert = convert
        # (Rust expression, JSON value) used by the generated tests
        self.sample = sample


class Struct:
    def __init__(self, name: str, doc_text: str, fields: list[Field], tool: str | None):
        self.name = name
        self.doc = doc_text
        self.fields = fields
        self.tool = tool

    @property
    def required(self) -> list[Field]:
        return [f for f in self.fields if f.required]

    @property
    def optional(self) -> list[Field]:
        return [f for f in self.fields if not f.required]


class RustGen:
    def __init__(self, spec: dict, helpers: dict):
        self.spec = spec
        self.tools = spec["tools"]
        self.helpers = helpers
        self.params = helpers["params"]
        self.tool_meta = helpers["tool_meta"]
        self.enums: dict[str, dict] = {}
        self.enum_order: list[str] = []
        self.structs: dict[str, Struct] = {}
        self.nested: dict[str, list[Struct]] = {}  # tool -> nested structs first used there
        self.uses_map = False
        self._value_sets: dict[str, set] = {}
        for tool in self.tools:
            if tool["name"] in CLIENT_METHODS:
                fail(f"tool {tool['name']!r} would shadow a hand-written Client method")
            self._scan(tool["inputSchema"], tool["name"])
        self.params_structs: dict[str, Struct] = {}
        for tool in self.tools:
            self._current_tool = tool["name"]
            self.nested.setdefault(tool["name"], [])
            fields = [Field(self, tool["name"], n, s, r) for n, s, r in self.params(tool)]
            if fields:
                name = pascal(tool["name"]) + "Params"
                text = f"Arguments of [`Client::{tool['name']}`] (tool `{tool['name']}`)."
                self.params_structs[tool["name"]] = self._register(Struct(name, text, fields, tool["name"]))

    # -- schema walking

    def _scan(self, schema: dict, owner: str) -> None:
        for prop, sub in (schema.get("properties") or {}).items():
            self._scan_value(sub, owner, prop)

    def _scan_value(self, schema: dict, owner: str, prop: str, struct_name: str | None = None) -> None:
        if self._is_string_enum(schema):
            self._value_sets.setdefault(prop, set()).add(tuple(schema["enum"]))
        if schema.get("type") == "array" and isinstance(schema.get("items"), dict):
            self._scan_value(schema["items"], owner, prop, self._struct_name(owner, prop, array=True))
        if schema.get("properties"):
            self._scan(schema, struct_name or self._struct_name(owner, prop, array=False))

    @staticmethod
    def _is_string_enum(schema: dict) -> bool:
        values = schema.get("enum")
        return bool(values) and schema.get("type", "string") == "string" and all(isinstance(v, str) for v in values)

    @staticmethod
    def _struct_name(owner: str, prop: str, array: bool) -> str:
        if (owner, prop) in STRUCT_NAMES:
            return STRUCT_NAMES[(owner, prop)]
        base = prop[:-1] if array and prop.endswith("s") and not prop.endswith("ss") else prop
        return pascal(owner) + pascal(base) + ("Item" if array and base == prop else "")

    def _enum_name(self, owner: str, prop: str) -> str:
        if (owner, prop) in ENUM_NAMES:
            return ENUM_NAMES[(owner, prop)]
        single = len(self._value_sets.get(prop, ())) == 1
        if single and prop in ENUM_NAMES:
            return ENUM_NAMES[prop]
        return pascal(prop) if single else pascal(owner) + pascal(prop)

    def _register(self, struct: Struct) -> Struct:
        if struct.name in self.structs or struct.name in self.enums:
            fail(f"two types are named {struct.name}; add an entry to STRUCT_NAMES")
        self.structs[struct.name] = struct
        return struct

    def rust_type(self, schema: dict, owner: str, prop: str, struct_name: str | None = None) -> Type:
        """The Rust type of property `prop` of `owner` (a tool or a nested struct)."""
        kind = schema.get("type")
        if self._is_string_enum(schema):
            name = self._enum_name(owner, prop)
            values = list(schema["enum"])
            known = self.enums.get(name)
            if known is None:
                self.enums[name] = {"values": values, "prop": prop, "used_by": [owner]}
                self.enum_order.append(name)
            elif known["values"] != values:
                fail(f"enum {name} is used for different values; add an entry to ENUM_NAMES")
            elif owner not in known["used_by"]:
                known["used_by"].append(owner)
            first = values[0]
            return Type(name, sample=(f"{name}::{pascal(first)}", first))
        if kind == "string":
            sample = "https://example.com/post" if schema.get("format") == "uri" else "sample"
            return Type("String", "impl Into<String>", "{v}.into()", (rust_str(sample), sample))
        if kind == "number":
            value = float(schema.get("minimum", 1))
            return Type("f64", sample=(repr(value), value))
        if kind == "integer":
            minimum = schema.get("minimum")
            value = int(minimum if minimum is not None else 1)
            rust = "u64" if minimum is not None and minimum >= 0 else "i64"
            return Type(rust, sample=(str(value), value))
        if kind == "boolean":
            return Type("bool", sample=("true", True))
        if kind == "array":
            item_struct = self._struct_name(owner, prop, array=True)
            item = self.rust_type(schema.get("items") or {}, owner, prop, item_struct)
            rust = f"Vec<{item.rust}>"
            sample = (f"[{item.sample[0]}]", [item.sample[1]])
            if item.rust == "String":
                return Type(rust, "impl IntoIterator<Item = impl Into<String>>", "{v}.into_iter().map(Into::into).collect()", sample)
            if item.arg == item.rust:
                return Type(rust, f"impl IntoIterator<Item = {item.rust}>", "{v}.into_iter().collect()", sample)
            return Type(rust, sample=(f"vec![{item.sample[0]}]", [item.sample[1]]))
        if kind == "object" and schema.get("properties"):
            if struct_name:
                return self._object(schema, prop, struct_name, f"One entry of `{prop}`")
            return self._object(schema, prop, self._struct_name(owner, prop, array=False), f"The `{prop}` object")
        if kind == "object":
            self.uses_map = True
            return Type("Map<String, Value>", sample=("Map::new()", {}))
        return Type("Value", sample=('Value::from("sample")', "sample"))

    def _object(self, schema: dict, prop: str, name: str, what: str) -> Type:
        if name not in self.structs:
            required = set(schema.get("required") or [])
            props = schema.get("properties", {})
            ordered = [n for n in props if n in required] + [n for n in props if n not in required]
            text = f"{what} in [`{pascal(self._current_tool)}Params`]."
            struct = self._register(Struct(name, text, [], None))
            struct.fields = [Field(self, name, n, props[n], n in required) for n in ordered]
            self.nested[self._current_tool].append(struct)
        struct = self.structs[name]
        args = [f.ty.sample[0] for f in struct.required]
        sample_json = {f.name: f.ty.sample[1] for f in struct.required}
        return Type(name, sample=(f"{name}::new({', '.join(args)})", sample_json))

    # --------------------------------------------------------------------------- library

    def library(self) -> str:
        h = self.helpers
        out = [
            f"// {h['HEADER']}",
            "",
            "//! Typed arguments and one method per tool of the RapidWorkers MCP server.",
            "",
            "use serde::{Deserialize, Serialize};",
            "use serde_json::{Map, Value};" if self._needs_map() else "use serde_json::Value;",
            "",
            "use crate::client::{to_arguments, Client};",
            "use crate::error::Result;",
            "",
            "/// The hosted RapidWorkers MCP endpoint, used unless `RAPIDWORKERS_MCP_URL` or",
            "/// [`ClientBuilder::base_url`](crate::ClientBuilder::base_url) says otherwise.",
            f"pub const SERVER_URL: &str = {rust_str(self.spec['server']['url'])};",
            "",
            "/// How the client treats a tool when a call fails or moves money.",
            "#[derive(Debug, Clone, Copy, PartialEq, Eq, Hash)]",
            "#[non_exhaustive]",
            "pub struct ToolMeta {",
            "    /// The tool only reads (`readOnlyHint`).",
            "    pub read_only: bool,",
            "    /// The tool takes an `idempotency_key`; the client creates one when it is left unset.",
            "    pub idempotency_key: bool,",
            "    /// Safe to resend after a timeout or a gateway error: nothing changes, or the server",
            "    /// dedupes the call by its idempotency key.",
            "    pub retry: bool,",
            "}",
            "",
            "/// The name of every tool, in the order of the server's `tools/list` answer.",
            "pub const TOOL_NAMES: &[&str] = &[",
        ]
        out += [f"    {rust_str(t['name'])}," for t in self.tools]
        out += [
            "];",
            "",
            "/// How the client treats the tool `name`, or `None` for a tool this version does not know.",
            "pub fn tool_meta(name: &str) -> Option<ToolMeta> {",
            "    let (read_only, idempotency_key, retry) = match name {",
        ]
        for tool in self.tools:
            meta = self.tool_meta(tool)
            flags = ", ".join("true" if meta[k] else "false" for k in ("read_only", "idempotency_key", "retry"))
            out.append(f"        {rust_str(tool['name'])} => ({flags}),")
        out += [
            "        _ => return None,",
            "    };",
            "    Some(ToolMeta {",
            "        read_only,",
            "        idempotency_key,",
            "        retry,",
            "    })",
            "}",
        ]
        for name in self.enum_order:
            out += [""] + self._enum(name)
        for tool in self.tools:
            struct = self.params_structs.get(tool["name"])
            if struct:
                out += [""] + self._struct(struct)
            for nested in self.nested.get(tool["name"], []):
                out += [""] + self._struct(nested)
        out += ["", "impl Client {"]
        for i, tool in enumerate(self.tools):
            if i:
                out.append("")
            out += self._method(tool)
        out.append("}")
        return "\n".join(out) + "\n"

    def _needs_map(self) -> bool:
        return self.uses_map or any(tool["name"] not in self.params_structs for tool in self.tools)

    def _enum(self, name: str) -> list[str]:
        info = self.enums[name]
        users = ", ".join(f"`{u}`" for u in info["used_by"])
        out = doc(f"Values of `{info['prop']}` ({users}).", "")
        out += [
            "#[derive(Debug, Clone, Copy, PartialEq, Eq, Hash, Serialize, Deserialize)]",
            "#[non_exhaustive]",
            f"pub enum {name} {{",
        ]
        variants = [pascal(v) for v in info["values"]]
        if len(set(variants)) != len(variants):
            fail(f"enum {name} has values that map to the same variant name")
        for value, variant in zip(info["values"], variants):
            out += [
                f"    /// `{json.dumps(value)}`",
                f"    #[serde(rename = {rust_str(value)})]",
                f"    {variant},",
            ]
        out += [
            "}",
            "",
            f"impl {name} {{",
            "    /// The value sent to the server.",
            "    pub const fn as_str(&self) -> &'static str {",
            "        match self {",
        ]
        out += [f"            Self::{variant} => {rust_str(value)}," for value, variant in zip(info["values"], variants)]
        out += [
            "        }",
            "    }",
            "}",
            "",
            f"impl std::fmt::Display for {name} {{",
            "    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {",
            "        f.write_str(self.as_str())",
            "    }",
            "}",
        ]
        return out

    @staticmethod
    def _field_doc(field: Field) -> str:
        text = field.schema.get("description") or f"`{field.name}`."
        if field.name == "idempotency_key":
            text = "Optional. Created automatically when omitted and reused on retries. " + text
        return text

    def _struct(self, struct: Struct) -> list[str]:
        derives = ["Debug", "Clone", "PartialEq"]
        if not struct.required:
            derives.append("Default")
        derives += ["Serialize", "Deserialize"]
        out = doc(struct.doc, "")
        out += [f"#[derive({', '.join(derives)})]", "#[non_exhaustive]", f"pub struct {struct.name} {{"]
        for field in struct.fields:
            out += doc(self._field_doc(field), "    ")
            if field.ident != field.name:
                out.append(f"    #[serde(rename = {rust_str(field.name)})]")
            if not field.required:
                out.append('    #[serde(skip_serializing_if = "Option::is_none")]')
            out.append(f"    pub {field.ident}: {field.rust},")
        out += ["}", "", f"impl {struct.name} {{"]
        # new(): required fields as arguments, optional ones unset
        if struct.required:
            names = ", ".join(f"`{f.name}`" for f in struct.required)
            out += doc(f"Arguments with the required {names}; optional fields start unset.", "    ")
        else:
            out += ["    /// Arguments with every optional field unset."]
        out.append("    #[must_use]")
        if len(struct.required) > 7:
            out.append("    #[allow(clippy::too_many_arguments)]")
        params = [f"{f.ident}: {f.ty.arg}" for f in struct.required]
        signature = f"    pub fn new({', '.join(params)}) -> Self {{"
        if len(signature) <= LINE_WIDTH:
            out.append(signature)
        else:
            out.append("    pub fn new(")
            out += [f"        {p}," for p in params]
            out.append("    ) -> Self {")
        if not struct.required:
            out.append("        Self::default()")
        else:
            inits = []
            for field in struct.fields:
                if not field.required:
                    inits.append(f"{field.ident}: None")
                elif field.ty.convert == "{v}":
                    inits.append(field.ident)
                else:
                    inits.append(f"{field.ident}: {field.ty.convert.format(v=field.ident)}")
            if len(", ".join(inits)) <= STRUCT_LIT_WIDTH:
                out.append(f"        Self {{ {', '.join(inits)} }}")
            else:
                out += ["        Self {"] + [f"            {init}," for init in inits] + ["        }"]
        out.append("    }")
        for field in struct.optional:
            value = field.ty.convert.format(v=field.ident)
            out += [""] + doc(f"Sets `{field.name}`: " + self._field_doc(field), "    ")
            out.append("    #[must_use]")
            signature = f"    pub fn {field.ident}(mut self, {field.ident}: {field.ty.arg}) -> Self {{"
            if len(signature) <= LINE_WIDTH:
                out.append(signature)
            else:
                out += [f"    pub fn {field.ident}(", "        mut self,", f"        {field.ident}: {field.ty.arg},", "    ) -> Self {"]
            assign = f"        self.{field.ident} = Some({value});"
            if len(assign) <= LINE_WIDTH:
                out.append(assign)
            else:
                out += [f"        self.{field.ident} = Some(", f"            {value},", "        );"]
            out += ["        self", "    }"]
        out.append("}")
        return out

    def _method(self, tool: dict) -> list[str]:
        name = tool["name"]
        meta = self.tool_meta(tool)
        if meta["read_only"]:
            note = "Read-only: retried automatically after network and gateway errors."
        elif meta["idempotency_key"]:
            note = (
                "Changes your account or moves money. The call carries an idempotency key (created when "
                "`idempotency_key` is unset) that is reused on every retry, so it never happens twice."
            )
        elif meta["retry"]:
            note = "Idempotent: retried automatically after network and gateway errors."
        else:
            note = (
                "Not resent after a network or gateway error, since it may already have run; "
                "only HTTP 429 and 503 answers, which come before the tool runs, are retried."
            )
        out = doc(tool.get("description", ""), "    ")
        out += ["    ///"] + doc(f"Tool `{name}`. {note}", "    ")
        struct = self.params_structs.get(name)
        if struct:
            out.append(f"    pub async fn {name}(&self, params: {struct.name}) -> Result<Value> {{")
            chain = f"self.call_tool({rust_str(name)}, to_arguments(&params)?)"
        else:
            out.append(f"    pub async fn {name}(&self) -> Result<Value> {{")
            chain = f"self.call_tool({rust_str(name)}, Value::Object(Map::new()))"
        if len(chain + ".await") <= CHAIN_WIDTH:
            out.append(f"        {chain}.await")
        else:
            out += [f"        {chain}", "            .await"]
        out.append("    }")
        return out

    # --------------------------------------------------------------------------- tests

    def tests(self) -> str:
        h = self.helpers
        body: list[str] = []
        for tool in self.tools:
            body += [""] + self._tool_test(tool)
        body += ["", "#[test]", "fn enums_use_the_wire_values() {"]
        for name in self.enum_order:
            for value in self.enums[name]["values"]:
                variant = f"{name}::{pascal(value)}"
                body += [
                    f"    assert_eq!({variant}.as_str(), {rust_str(value)});",
                    f"    assert_eq!(json!({variant}), json!({rust_str(value)}));",
                ]
        body.append("}")
        names = ["json"] + [n for n in ("Map", "Value") if any(f"{n}::" in line or f"{n}(" in line for line in body)]
        out = [
            f"// {h['HEADER']}",
            "",
            "//! Every tool in spec/tools.json has a typed method that sends the tool's name and",
            "//! arguments, with an idempotency key where the tool takes one.",
            "",
            "mod common;",
            "",
            "// Written by scripts/gen_rust.py, which owns its layout",
            "#[rustfmt::skip]",
            "mod tools {",
            "    use rapidworkers::*;",
            f"    use serde_json::{{{', '.join(names)}}};",
            "",
            "    use crate::common;",
        ]
        out += [("    " + line) if line else "" for line in body]
        out.append("}")
        return "\n".join(out) + "\n"

    def _tool_test(self, tool: dict) -> list[str]:
        name = tool["name"]
        meta = self.tool_meta(tool)
        struct = self.params_structs.get(name)
        out = [
            "#[tokio::test]",
            f"async fn {name}_sends_its_arguments() -> Result<()> {{",
            "    let mock = common::Mock::start().await;",
            "    mock.answer(common::reply(common::structured(json!({ \"ok\": true }))))",
            "        .await;",
            "    let client = mock.client();",
        ]
        expected: dict = {}
        if struct:
            args = []
            for field in struct.required:
                rust_expr, value = field.ty.sample
                args.append(rust_expr)
                expected[field.name] = value
            out += call(f"let params = {struct.name}::new", args, "    ", ";")
            out.append(f"    let answer = client.{name}(params).await?;")
        else:
            out.append(f"    let answer = client.{name}().await?;")
        out += [
            "    assert_eq!(answer, json!({ \"ok\": true }));",
            f"    let mut arguments = mock.tool_arguments({rust_str(name)}).await;",
        ]
        if meta["idempotency_key"]:
            out += [
                '    let key = arguments.remove("idempotency_key").unwrap_or_default();',
                '    assert!(key.as_str().unwrap_or_default().starts_with("sdk-"));',
            ]
        else:
            out.append('    assert!(arguments.remove("idempotency_key").is_none());')
        if expected:
            out.append("    let expected = json!({")
            items = list(expected.items())
            for i, (key, value) in enumerate(items):
                comma = "," if i < len(items) - 1 else ""
                out.append(f"        {json.dumps(key)}: {json.dumps(value)}{comma}")
            out.append("    });")
            out.append("    assert_eq!(Value::Object(arguments), expected);")
        else:
            out.append("    assert!(arguments.is_empty());")
        out += ["    Ok(())", "}"]
        return out


def outputs(spec: dict, root: Path, helpers: dict) -> dict:
    gen = RustGen(spec, helpers)
    crate = root / "rust" / "rapidworkers"
    return {
        crate / "src" / "generated.rs": gen.library,
        crate / "tests" / "tools.rs": gen.tests,
        root / "rust" / "rapidworkers-mcp" / "src" / "tools.json": helpers["spec_json"],
    }
