// Command api serves the read model behind the operations dashboard.
//
// Separate from cmd/ingest on purpose: this process only ever reads, so it can
// be scaled, cached and deployed on its own, and a slow dashboard query can
// never hold up a webhook the platform has seconds to accept.
package main

import (
	"context"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"golang.org/x/sync/errgroup"

	"github.com/regisoliveira/dispute-router/cmd/internal/boot"
	"github.com/regisoliveira/dispute-router/internal/api"
	"github.com/regisoliveira/dispute-router/internal/awsx"
	"github.com/regisoliveira/dispute-router/internal/config"
	"github.com/regisoliveira/dispute-router/internal/debugx"
	"github.com/regisoliveira/dispute-router/internal/httpx"
)

func main() {
	logger := boot.Logger(true)
	if err := run(logger); err != nil {
		logger.Error("fatal", "error", err)
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

	pool, err := boot.Postgres(ctx, cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer pool.Close()

	rdb, err := boot.Redis(ctx, cfg.RedisURL)
	if err != nil {
		return err
	}
	defer func() { _ = rdb.Close() }()

	awsCfg, err := awsx.Load(ctx, cfg.AWS.Config)
	if err != nil {
		return err
	}

	// Fifteen minutes: long enough to pick a file and upload it over a bad
	// connection, short enough that a URL pasted into a chat is dead by the
	// time anyone else opens it.
	evidence := api.NewEvidence(
		awsx.S3(awsCfg),
		cfg.AWS.EvidenceBucket,
		15*time.Minute,
	)

	handler := api.NewHandler(api.NewStore(pool), rdb, evidence, logger)

	// Applied outermost-first: request id and logging wrap everything, then
	// CORS answers preflights before anything is routed. The request deadline
	// is not here: Routes applies it per route, so that the SSE stream can be
	// registered unbounded without a middleware having to recognise its path.
	var root http.Handler = handler.Routes(cfg.API.RequestTimeout)
	root = api.CORS(cfg.API.CORSOrigins)(root)
	root = httpx.Observe(logger)(root)

	server := &http.Server{
		Addr:              cfg.API.Addr,
		Handler:           root,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		// No WriteTimeout: the CSV export and the SSE stream are both
		// long-lived by design, and a write deadline would cut them off
		// mid-response with no way to tell the client why.
		IdleTimeout: 120 * time.Second,
	}

	group, groupCtx := errgroup.WithContext(ctx)

	// Runtime diagnostics on loopback, off unless PPROF_ADDR is set. On by
	// convention at 127.0.0.1:6060 for this binary.
	group.Go(func() error { return debugx.Serve(groupCtx, cfg.PprofAddr, logger) })

	logger.Info("api listening", "addr", cfg.API.Addr, "cors", cfg.API.CORSOrigins)
	boot.ServeHTTP(groupCtx, group, server, cfg.ShutdownTimeout, logger)

	if err := group.Wait(); err != nil {
		return err
	}
	logger.Info("stopped cleanly")
	return nil
}
