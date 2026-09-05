// Command api serves the read model behind the operations dashboard.
//
// Separate from cmd/ingest on purpose: this process only ever reads, so it can
// be scaled, cached and deployed on its own, and a slow dashboard query can
// never hold up a webhook the platform has seconds to accept.
package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"
	"golang.org/x/sync/errgroup"

	"github.com/regisoliveira/dispute-router/internal/api"
	"github.com/regisoliveira/dispute-router/internal/awsx"
	"github.com/regisoliveira/dispute-router/internal/config"
	"github.com/regisoliveira/dispute-router/internal/httpx"
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	if err := run(logger); err != nil {
		logger.Error("fatal", "error", err)
		os.Exit(1)
	}
}

func run(logger *slog.Logger) error {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	cfg, err := config.Load(".env")
	if err != nil {
		return err
	}

	pool, err := pgxpool.New(ctx, cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer pool.Close()
	if err := pool.Ping(ctx); err != nil {
		return err
	}

	redisOptions, err := redis.ParseURL(cfg.RedisURL)
	if err != nil {
		return err
	}
	rdb := redis.NewClient(redisOptions)
	defer func() { _ = rdb.Close() }()
	if err := rdb.Ping(ctx).Err(); err != nil {
		return err
	}

	awsCfg, err := awsx.Load(ctx, awsx.Config{
		Region:          cfg.AWSRegion,
		Endpoint:        cfg.AWSEndpoint,
		AccessKeyID:     cfg.AWSAccessKey,
		SecretAccessKey: cfg.AWSSecretKey,
	})
	if err != nil {
		return err
	}

	// Fifteen minutes: long enough to pick a file and upload it over a bad
	// connection, short enough that a URL pasted into a chat is dead by the
	// time anyone else opens it.
	evidence := api.NewEvidence(
		awsx.S3(awsCfg, cfg.AWSEndpoint),
		cfg.S3EvidenceBucket,
		15*time.Minute,
	)

	handler := api.NewHandler(api.NewStore(pool), rdb, evidence, logger)

	// Applied outermost-first: request id and logging wrap everything, then
	// CORS answers preflights before the timeout clock starts.
	var root http.Handler = handler.Routes()
	root = api.Timeout(cfg.RequestTimeout)(root)
	root = api.CORS(cfg.CORSOrigins)(root)
	root = httpx.Middleware(logger)(root)

	server := &http.Server{
		Addr:              cfg.APIAddr,
		Handler:           root,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		// No WriteTimeout: the CSV export and the SSE stream are both
		// long-lived by design, and a write deadline would cut them off
		// mid-response with no way to tell the client why.
		IdleTimeout: 120 * time.Second,
	}

	group, groupCtx := errgroup.WithContext(ctx)

	group.Go(func() error {
		logger.Info("api listening", "addr", cfg.APIAddr, "cors", cfg.CORSOrigins)
		if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			return err
		}
		return nil
	})

	group.Go(func() error {
		<-groupCtx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), cfg.ShutdownTimeout)
		defer cancel()
		logger.Info("shutting down", "grace", cfg.ShutdownTimeout.String())
		return server.Shutdown(shutdownCtx)
	})

	if err := group.Wait(); err != nil {
		return err
	}
	logger.Info("stopped cleanly")
	return nil
}
