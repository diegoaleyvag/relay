// Command relay runs the Relay control-room web server and hosts the run engine.
//
// Relay is a bounded reliability lab: it demonstrates that useful work survives
// timeouts, malformed tool responses, duplicate requests, process restarts,
// permission denial and human escalation. This binary is the composition root
// that wires the durable store, the MCP tool client, telemetry and the
// server-rendered control room together. The scaffold prints a banner until the
// engine and web adapter land (M5/M7).
package main

import "fmt"

func main() {
	fmt.Println("relay: control room (scaffold) — see docs/DEMO.md")
}
