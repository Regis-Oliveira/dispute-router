// Command mcp serves the dispute read model over the Model Context Protocol.
//
// Runs as a subprocess of an MCP client - Claude Code, Claude Desktop, an IDE -
// and speaks JSON-RPC over stdin and stdout. That transport choice has one
// consequence worth knowing before debugging anything: stdout IS the protocol,
// so a stray fmt.Println anywhere in the process corrupts the stream. Every log
// line in this binary goes to stderr, which the client shows separately.
package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/regisoliveira/dispute-router/cmd/internal/boot"
	"github.com/regisoliveira/dispute-router/internal/api"
	"github.com/regisoliveira/dispute-router/internal/config"
	"github.com/regisoliveira/dispute-router/internal/mcpserver"
)

const version = "0.1.0"

func main() {
	// stderr, never stdout. See the package comment.
	logger := boot.Logger(false)

	// Sentry batches, so an event only leaves on a flush. This one covers the
	// ordinary exit and a panic unwinding out of run.
	defer boot.FlushSentry()

	if err := run(logger); err != nil {
		logger.Error("fatal", "error", err)
		// And this one covers the exit that matters, because os.Exit skips the
		// deferred call above and the line just logged is the one that says
		// why the process is stopping.
		boot.FlushSentry()
		os.Exit(1)
	}
}

func run(logger *slog.Logger) error {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	cfg, err := config.Load()
	if err != nil {
		return err
	}
	if err := cfg.RequireDatabase(); err != nil {
		return err
	}
	if err := boot.Sentry(cfg.Sentry.DSN, cfg.Sentry.Environment, cfg.Sentry.Release); err != nil {
		return err
	}

	pool, err := boot.Postgres(ctx, cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer pool.Close()

	server := mcp.NewServer(&mcp.Implementation{
		Name:        "dispute-router",
		Title:       "Dispute Router",
		Version:     version,
		Description: "Read-only access to the dispute queue, its history and its ledger.",
	}, nil)

	mcpserver.New(api.NewStore(pool)).Register(server)

	logger.Info("mcp server ready", "version", version, "transport", "stdio")

	// Blocks until the client disconnects or the process is signalled.
	if err := server.Run(ctx, &mcp.StdioTransport{}); err != nil {
		if ctx.Err() != nil {
			return nil
		}
		return err
	}
	return nil
}
