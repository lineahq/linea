// Command linea-server serves the debugger's team server: the HTTP API, the
// web client and an optional MCP endpoint over shared storage. It links no
// Collector: connected workers run every request.
//
// Usage:
//
//	linea-server --config <file> [--addr 127.0.0.1:7070] [--tls-cert-file FILE --tls-key-file FILE | --allow-http] [--mcp]
package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	"github.com/lineahq/linea/internal/debugger/server"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	code := server.Main(ctx, os.Args[1:], os.Stdout, os.Stderr)
	stop()
	os.Exit(code)
}
