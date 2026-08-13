package mcp

import (
	"context"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// InMemory wires a Relay MCP server (backed by src) to a client over an
// in-process transport pair, with no subprocess or network hop. It is meant
// for tests and for embedding the tool server directly in a process (e.g. a
// single-binary demo) that also drives the engine.
//
// The returned port's Session is already connected and ready for
// MCPToolPort.Invoke. closeFn tears down both sides: it closes the client
// session (a graceful shutdown the server observes) and then cancels the
// context the server is running under, waiting for the server's Run call to
// return before returning itself. closeFn is safe to call exactly once; it is
// not idempotent (Session.Close is, but this also waits on the server
// goroutine, and reading from an already-drained channel would block).
func InMemory(ctx context.Context, src Sources) (port *MCPToolPort, closeFn func() error, err error) {
	serverTransport, clientTransport := mcpsdk.NewInMemoryTransports()

	srv := NewServer(src)

	runCtx, cancel := context.WithCancel(ctx)
	serverDone := make(chan error, 1)
	go func() {
		serverDone <- srv.Run(runCtx, serverTransport)
	}()

	client := mcpsdk.NewClient(&mcpsdk.Implementation{
		Name:    "relay-client",
		Version: "0.1.0",
	}, nil)

	session, connErr := client.Connect(ctx, clientTransport, nil)
	if connErr != nil {
		cancel()
		<-serverDone
		return nil, nil, connErr
	}

	port = &MCPToolPort{Session: session}
	closeFn = func() error {
		closeErr := session.Close()
		cancel()
		<-serverDone
		return closeErr
	}
	return port, closeFn, nil
}
