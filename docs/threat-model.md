# Threat model and privacy/redaction policy

Relay is a local, single-operator reliability lab. It is not multi-tenant and is
not exposed to the public internet. The threat model is scoped accordingly, but
the reliability and privacy properties are enforced by construction, not by
convention.

## Trust boundaries

```
operator ──HTTP──> control room ──> engine ──ToolPort──> MCP client ══MCP══> MCP server ──> local corpus
                        │                │                                      │
                        └── reads ──> SQLite store (durable state) <── writes ──┘
                                         │
                                    OTel exporter (stdout/file)
```

- **Untrusted input** enters only at two places: HTTP form input at the control
  room, and tool responses crossing the MCP boundary. Both are validated and size
  limited at the adapter before reaching the domain.
- The **domain core** is pure and performs no I/O; it cannot be induced to reach
  the network, the filesystem, or the database. `make boundary` enforces this.
- **Model/tool output never authorizes a privileged action.** The planner is
  scripted and deterministic; tool results only advance a fixed state machine.

## Assets and controls

| Asset | Threat | Control |
|-------|--------|---------|
| Durable run state | corruption / duplicate side effects | version CAS rejects stale writers; `UNIQUE(idempotency_key)`; intent→confirm ledger |
| Source content | leaking into logs/telemetry/UI | telemetry attributes are an allowlist set only via typed constructors (no `SetString`); UI view models carry metadata only; `html/template` escapes all output |
| Tool surface | invocation of an unintended tool | allowlist by registration (fail closed) + independent adapter allowlist before `CallTool` |
| Corpus files | path traversal / oversized reads | `fs.ValidPath` + clean-and-prefix checks; `io.LimitReader` at 64 KiB |
| Control-room actions | illegal state transitions, CSRF | every POST re-validates the transition (409 on illegal); same-site cookies for the localhost lab |

## Privacy / redaction policy

- **Telemetry** records IDs, types, durations, outcomes, error codes and hashes —
  never source content, tool arguments or finding text. The allowlist is closed
  and asserted by a test (`internal/telemetry`).
- **SQLite and logs contain no credentials.** There are none in the system.
- **Event history is redacted**: the reducer only ever builds evidence from
  metadata, and a `LimitRedactor` fingerprints any over-long string as
  defense-in-depth before persistence.
- **The UI escapes all displayed payloads** and physically excludes content
  fields from its view models.
- **Idempotency keys** appear in telemetry only as a truncated fingerprint.

## Out of scope

Power-loss/OS-crash durability (Relay uses `synchronous=NORMAL`, which is durable
across process kill — the modelled failure — but not a power cut), multi-user
authz, network transport hardening, and secret management (there are no secrets).
