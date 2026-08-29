#!/usr/bin/env python3
"""Offline checks for the static preview and its portfolio manifest."""
import hashlib
import json
import re
from pathlib import Path
from urllib.parse import urlparse

ROOT = Path(__file__).resolve().parent.parent
PREVIEW = ROOT / "preview"


def require(condition, message):
    if not condition:
        raise SystemExit(f"brand-check: {message}")


def json_type(value, expected):
    return {
        "object": lambda: isinstance(value, dict),
        "array": lambda: isinstance(value, list),
        "string": lambda: isinstance(value, str),
        "null": lambda: value is None,
    }.get(expected, lambda: False)()


def validate(value, schema, path="$"):
    if "oneOf" in schema:
        matches = 0
        for candidate in schema["oneOf"]:
            try:
                validate(value, candidate, path)
                matches += 1
            except ValueError:
                pass
        if matches != 1:
            raise ValueError(f"{path}: expected exactly one matching oneOf schema")
        return

    if "type" in schema and not json_type(value, schema["type"]):
        raise ValueError(f"{path}: expected {schema['type']}")
    if "const" in schema and value != schema["const"]:
        raise ValueError(f"{path}: expected constant {schema['const']!r}")
    if "enum" in schema and value not in schema["enum"]:
        raise ValueError(f"{path}: value is not in enum")
    if isinstance(value, str):
        if "minLength" in schema and len(value) < schema["minLength"]:
            raise ValueError(f"{path}: shorter than minLength")
        if "maxLength" in schema and len(value) > schema["maxLength"]:
            raise ValueError(f"{path}: longer than maxLength")
        if "pattern" in schema and not re.search(schema["pattern"], value):
            raise ValueError(f"{path}: does not match pattern")
        if schema.get("format") == "uri":
            parsed = urlparse(value)
            if not parsed.scheme:
                raise ValueError(f"{path}: invalid URI")
    if isinstance(value, list) and "items" in schema:
        for index, item in enumerate(value):
            validate(item, schema["items"], f"{path}[{index}]")
    if isinstance(value, dict):
        required = schema.get("required", [])
        for key in required:
            if key not in value:
                raise ValueError(f"{path}: missing required property {key!r}")
        properties = schema.get("properties", {})
        if schema.get("additionalProperties") is False:
            unknown = set(value) - set(properties)
            if unknown:
                raise ValueError(f"{path}: unknown properties {sorted(unknown)!r}")
        for key, item in value.items():
            if key in properties:
                validate(item, properties[key], f"{path}.{key}")


def main():
    manifest = json.loads((ROOT / "portfolio.project.json").read_text())
    snapshot = json.loads((PREVIEW / "brand-contract.snapshot.json").read_text())
    schema = json.loads((PREVIEW / "schema/portfolio.project.schema.json").read_text())
    html = (PREVIEW / "index.html").read_text().lower()
    css = (PREVIEW / "preview.css").read_text().lower()

    require(snapshot["version"] == "1.0.0", "brand snapshot version must be 1.0.0")
    for name, entry in snapshot["files"].items():
        path = ROOT / entry["path"]
        require(path.is_file(), f"missing vendored {name} contract file")
        actual = hashlib.sha256(path.read_bytes()).hexdigest()
        require(actual == entry["sha256"], f"{name} contract hash mismatch")
    try:
        validate(manifest, schema)
    except ValueError as error:
        raise SystemExit(f"brand-check: manifest schema validation failed: {error}")
    require("<form" not in html and "method=\"post\"" not in html and "hx-post" not in html, "static preview must not contain POST controls")
    require("static preview" in html and "make run" in html, "preview must disclose its limitation and local demo")
    require("public sans" in css and "martian mono" in css, "preview must declare contract typography")
    print("brand-check ok: vendored contract bytes and canonical manifest schema verified")


if __name__ == "__main__":
    main()
