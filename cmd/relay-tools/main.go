// Command relay-tools runs the Relay MCP server exposing the four synthetic
// research tools (list_sources, read_source, record_finding,
// request_human_review) over stdio.
//
// The tool set is an allowlist: only these four names are registered, and any
// other tool name fails closed. The corpus is the small, local, synthetic and
// redistributable source set embedded in internal/corpus.
package main

import (
	"context"
	"log"
	"os"
	"os/signal"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/diegoaleyvag/relay/internal/corpus"
	"github.com/diegoaleyvag/relay/internal/mcp"
)

func main() {
	log.SetFlags(0)
	log.SetPrefix("relay-tools: ")

	crp, err := corpus.Load()
	if err != nil {
		log.Fatalf("load corpus: %v", err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	srv := mcp.NewServer(crp)
	if err := srv.Run(ctx, &mcpsdk.StdioTransport{}); err != nil {
		log.Fatalf("serve: %v", err)
	}
}
