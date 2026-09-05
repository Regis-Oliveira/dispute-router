// Command ingest receives dispute webhooks from the payment processor.
package main

import (
	"context"
	"encoding/json"
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
	"github.com/regisoliveira/dispute-router/internal/ingest"
	"github.com/regisoliveira/dispute-router/internal/outbox"
	"github.com/regisoliveira/dispute-router/internal/secrets"
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))

	if err := run(logger); err != nil {
		logger.Error("fatal", "error", err)
		os.Exit(1)
	}
}

func run(logger *slog.Logger) error {
	// Cancelled on SIGINT/SIGTERM, which is what unwinds everything below.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	cfg, err := config.Load(dotenvPath())
	if err != nil {
		return err
	}

	pool, err := pgxpool.New(ctx, cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer pool.Close()

	// Fail at startup rather than on the first request, so a bad DSN is a crash
	// loop instead of a stream of 500s.
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

	// One AWS config, built before either the secret resolver or the outbox
	// publisher asks for a client.
	awsCfg, err := awsx.Load(ctx, awsx.Config{
		Region:          cfg.AWSRegion,
		Endpoint:        cfg.AWSEndpoint,
		AccessKeyID:     cfg.AWSAccessKey,
		SecretAccessKey: cfg.AWSSecretKey,
	})
	if err != nil {
		return err
	}

	store := ingest.NewStore(pool)

	// The database source keeps the system runnable with no AWS at all, and a
	// secret store you cannot fall back from is a single point of failure
	// wearing a security badge.
	var resolver secrets.Resolver = secrets.FromDatabase{Pool: pool}
	if cfg.WebhookSecretSource == "secretsmanager" {
		resolver = secrets.NewFromManager(
			awsx.SecretsManager(awsCfg, cfg.AWSEndpoint),
			cfg.WebhookSecretID,
			cfg.WebhookSecretTTL,
		)
	}
	logger.Info("webhook secrets", "source", cfg.WebhookSecretSource,
		"rotation_latency", cfg.WebhookSecretTTL.String())

	handler := ingest.NewHandler(ingest.HandlerOptions{
		Store:           store,
		Secrets:         resolver,
		Guard:           ingest.NewGuard(rdb, cfg.IdempotencyTTL),
		MerchantLimiter: ingest.NewLimiter(rdb, "ratelimit:merchant", cfg.RateLimitPerMinute, cfg.RateLimitBurst),
		IPLimiter:       ingest.NewLimiter(rdb, "ratelimit:ip", cfg.IPRateLimitPerMinute, cfg.IPRateLimitBurst),
		Tolerance:       cfg.SignatureTolerance,
		MaxBodyBytes:    cfg.MaxBodyBytes,
		Logger:          logger,
	})

	mux := http.NewServeMux()
	// Method-prefixed patterns are Go 1.22+; a GET to this path gets a 405 from
	// the mux rather than reaching the handler.
	mux.Handle("POST /webhooks/processor", handler)

	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	})

	// Liveness says the process is up; readiness says it can actually serve.
	// Conflating them gets a pod with a dead database left in the load balancer.
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, r *http.Request) {
		checks := map[string]string{"postgres": "ok", "redis": "ok"}
		status := http.StatusOK

		if err := store.Ping(r.Context()); err != nil {
			checks["postgres"] = err.Error()
			status = http.StatusServiceUnavailable
		}
		if err := rdb.Ping(r.Context()).Err(); err != nil {
			checks["redis"] = err.Error()
			status = http.StatusServiceUnavailable
		}

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(checks)
	})

	server := &http.Server{
		Addr:              cfg.Addr,
		Handler:           httpx.Middleware(logger)(mux),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      15 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	// SQS is now the real transport; Live still broadcasts each message to the
	// dashboard's SSE feed on the side, best-effort.
	//
	// The relay above this did not change at all. That is what the outbox
	// pattern bought: the queue underneath it was always a swappable detail.
	publisher := outbox.Live{
		Next: outbox.SQSPublisher{
			Client:   awsx.SQS(awsCfg, cfg.AWSEndpoint),
			QueueURL: cfg.SQSQueueURL,
		},
		Client:  rdb,
		Channel: api.LiveChannel,
		Logger:  logger,
	}
	relay := outbox.NewRelay(pool, publisher, cfg.OutboxPollInterval, cfg.OutboxBatchSize, logger)

	group, groupCtx := errgroup.WithContext(ctx)

	group.Go(func() error {
		logger.Info("ingest listening", "addr", cfg.Addr)
		if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			return err
		}
		return nil
	})

	group.Go(func() error {
		return relay.Run(groupCtx)
	})

	group.Go(func() error {
		<-groupCtx.Done()
		// A fresh context: groupCtx is already cancelled, and Shutdown needs a
		// live one to give in-flight requests their grace period.
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

// dotenvPath resolves the repo-root .env relative to the working directory the
// Makefile runs from (services/ingest).
func dotenvPath() string {
	if explicit := os.Getenv("DOTENV_PATH"); explicit != "" {
		return explicit
	}
	return ".env"
}
