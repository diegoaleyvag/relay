// Command relay-tools runs the Relay MCP server exposing the four synthetic
// research tools (list_sources, read_source, record_finding,
// request_human_review) over stdio.
//
// The tool set is an allowlist: only these four names are registered, and any
// other tool name fails closed. The scaffold prints a banner until the MCP
// server adapter lands (M4).
package main

import "fmt"

func main() {
	fmt.Println("relay-tools: MCP server (scaffold) — stdio transport")
}
