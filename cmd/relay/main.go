// Command relay runs the Relay control-room web server and hosts the run engine.
//
// Relay is a bounded reliability lab: it demonstrates that useful work survives
// timeouts, malformed tool responses, duplicate requests, process restarts and
// human escalation. This binary is the composition root that wires the durable
// SQLite store, the MCP tool client, OpenTelemetry and the server-rendered HTMX
// control room together, and resumes any runs a previous process left in flight.
//
// Configuration (environment):
//
//	RELAY_ADDR    listen address (default 127.0.0.1:8080)
//	RELAY_DB      SQLite path    (default relay.db)
//	RELAY_TRACES  off | stdout   (default off)
package main

import (
	"context"
	"errors"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/diegoaleyvag/relay/internal/core"
	"github.com/diegoaleyvag/relay/internal/corpus"
	"github.com/diegoaleyvag/relay/internal/engine"
	"github.com/diegoaleyvag/relay/internal/mcp"
	"github.com/diegoaleyvag/relay/internal/scenarios"
	"github.com/diegoaleyvag/relay/internal/store/sqlite"
	"github.com/diegoaleyvag/relay/internal/telemetry"
	"github.com/diegoaleyvag/relay/internal/web"
)

func main() {
	log.SetFlags(log.LstdFlags | log.Lmsgprefix)
	log.SetPrefix("relay: ")

	addr := env("RELAY_ADDR", "127.0.0.1:8080")
	dbPath := env("RELAY_DB", "relay.db")
	tracesMode := telemetry.Mode(env("RELAY_TRACES", string(telemetry.ModeOff)))

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// Durable state.
	store, err := sqlite.Open(dbPath)
	if err != nil {
		log.Fatalf("open store %q: %v", dbPath, err)
	}
	defer func() { _ = store.Close() }()

	// Corpus + MCP tool server over the in-memory transport (single process).
	crp, err := corpus.Load()
	if err != nil {
		log.Fatalf("load corpus: %v", err)
	}
	toolPort, closeMCP, err := mcp.InMemory(ctx, crp)
	if err != nil {
		log.Fatalf("mcp in-memory: %v", err)
	}
	defer func() { _ = closeMCP() }()
	// Defense-in-depth: reject any non-allowlisted tool at the adapter, before a
	// CallTool is ever issued (fail closed).
	toolPort.Allow = map[core.ToolName]bool{
		core.ToolListSources:   true,
		core.ToolReadSource:    true,
		core.ToolRecordFinding: true,
		core.ToolRequestReview: true,
	}

	// Telemetry.
	prov, shutdownTel, err := telemetry.NewProvider(ctx, telemetry.Config{Mode: tracesMode, Writer: os.Stderr})
	if err != nil {
		log.Fatalf("telemetry: %v", err)
	}
	defer func() { _ = shutdownTel(context.Background()) }()

	cfg := engine.DefaultConfig()
	cfg.PerCallTimeout = 300 * time.Millisecond

	runner := newRunner(store, toolPort, prov, cfg)
	runner.resumeAll() // recover runs left non-terminal by a previous process

	srv := web.New(store, runner, webScenarios())
	httpSrv := &http.Server{
		Addr:              addr,
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 5 * time.Second,
	}

	go func() {
		log.Printf("control room on http://%s", addr)
		if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("serve: %v", err)
		}
	}()

	<-ctx.Done()
	log.Print("shutting down")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := httpSrv.Shutdown(shutdownCtx); err != nil {
		log.Printf("http shutdown: %v", err)
	}
}

// webScenarios projects the demo scenarios onto the control room's view.
func webScenarios() []web.Scenario {
	src := scenarios.All()
	out := make([]web.Scenario, 0, len(src))
	for _, s := range src {
		out = append(out, web.Scenario{Name: s.Name, Label: s.Label, Description: s.Description})
	}
	return out
}

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
