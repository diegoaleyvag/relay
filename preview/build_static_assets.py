#!/usr/bin/env python3
"""Render static preview documentation and a deterministic favicon."""

from __future__ import annotations

import argparse
import html
import struct
from pathlib import Path

ROOT = Path(__file__).resolve().parent.parent
PREVIEW = ROOT / "preview"
DOCS = {
    "methodology.html": ("Failure semantics", ROOT / "docs" / "failure-semantics.md"),
    "demo.html": ("Local Relay demo", ROOT / "docs" / "DEMO.md"),
}


def document(title: str, source: str) -> bytes:
    escaped = html.escape(source)
    return f"""<!doctype html>
<html lang="en">
<head>
  <meta charset="utf-8">
  <meta name="viewport" content="width=device-width, initial-scale=1">
  <meta name="description" content="Static Relay documentation for local use.">
  <title>{title} | Relay preview</title>
  <link rel="icon" href="favicon.ico" sizes="16x16" type="image/x-icon">
  <link rel="stylesheet" href="preview.css">
</head>
<body>
  <header>
    <a class="brand" href="./">Relay</a>
    <span>Five Decisions · Reliability</span>
  </header>
  <main>
    <section class="hero">
      <p class="eyebrow">Local documentation</p>
      <h1>{title}</h1>
      <p class="notice"><strong>Static preview.</strong> This documentation is fixed sample content. The interactive control room remains local-only.</p>
      <p><a href="https://diegoaleyvag.github.io/">Return to Portfolio</a> · <a href="methodology.html">Failure semantics</a> · <a href="demo.html">Local demo</a></p>
    </section>
    <section class="source-document" aria-label="{title} source">
      <pre>{escaped}</pre>
    </section>
  </main>
  <footer>Relay · static Five Decisions sample · versioned brand snapshot included</footer>
</body>
</html>
""".encode()


def favicon() -> bytes:
    width = height = 16
    xor = bytes((0x34, 0x3B, 0x17, 0xFF)) * (width * height)
    and_mask = bytes(64)
    bitmap = struct.pack(
        "<IiiHHIIiiII",
        40,
        width,
        height * 2,
        1,
        32,
        0,
        len(xor) + len(and_mask),
        0,
        0,
        0,
        0,
    ) + xor + and_mask
    directory = struct.pack("<HHH", 0, 1, 1)
    entry = struct.pack("<BBBBHHII", width, height, 0, 0, 1, 32, len(bitmap), 22)
    return directory + entry + bitmap


def expected() -> dict[Path, bytes]:
    output = {PREVIEW / "favicon.ico": favicon()}
    for filename, (title, source) in DOCS.items():
        output[PREVIEW / filename] = document(title, source.read_text())
    return output


def main() -> None:
    parser = argparse.ArgumentParser()
    parser.add_argument("--check", action="store_true")
    args = parser.parse_args()
    stale = [path for path, contents in expected().items() if not path.is_file() or path.read_bytes() != contents]
    if args.check:
        if stale:
            raise SystemExit("preview assets are stale: " + ", ".join(path.name for path in stale))
        print("preview assets ok: documentation and favicon are deterministic")
        return
    for path, contents in expected().items():
        path.write_bytes(contents)
    print("preview assets generated: documentation and favicon")


if __name__ == "__main__":
    main()
