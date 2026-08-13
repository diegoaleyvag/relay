# ADR 0004 — Pure-Go SQLite driver

## Status
Accepted.

## Context
Relay needs an embedded SQL database. The two mainstream Go options are the cgo
driver `github.com/mattn/go-sqlite3` and the pure-Go `modernc.org/sqlite`.

## Decision
Use **`modernc.org/sqlite` v1.56.0** (pure Go, no cgo).

Rationale, weighed against the project's reliability-and-reproducibility thesis:

- **Reproducible, hermetic builds.** `CGO_ENABLED=0`, a single static binary, no
  C toolchain or `libsqlite` version drift between a laptop and CI.
- **Race detector without a C toolchain.** The `-race` run is a headline
  deliverable; cgo would leave the busiest I/O path opaque to the detector.
- **Trivial cross-compilation and vendoring.**

The trade-off — modernc is somewhat slower and single-writer-sensitive — is
irrelevant for a bounded lab running one run at a time, and is mitigated with WAL
plus a single-writer connection pool.

## Consequences
- Binaries build cgo-free (`make build` sets `CGO_ENABLED=0`); tests keep cgo
  enabled only because the race detector requires it.
- DSN pragmas (`journal_mode=WAL`, `busy_timeout`, `foreign_keys`,
  `synchronous=NORMAL`) are set via `_pragma=` query parameters.
