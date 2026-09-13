// Command ingest receives dispute webhooks from the payment processor.
package main

import (
	"context"
	"encoding/json"
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
	"github.com/regisoliveira/dispute-router/internal/ingest"
	"github.com/regisoliveira/dispute-router/internal/outbox"
	"github.com/regisoliveira/dispute-router/internal/secrets"
)

func main() {
	logger := boot.Logger(true)

	if err := run(logger); err != nil {
		logger.Error("fatal", "error", err)
		os.Exit(1)
	}
}

func run(logger *slog.Logger) error {
	// Cancelled on SIGINT/SIGTERM, which is what unwinds everything below.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	cfg, err := config.Load()
	if err != nil {
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

	// One AWS config, built before either the secret resolver or the outbox
	// publisher asks for a client.
	awsCfg, err := awsx.Load(ctx, cfg.AWS())
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
			awsx.SecretsManager(awsCfg),
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
		Handler:           httpx.Observe(logger)(mux),
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
			Client:   awsx.SQS(awsCfg),
			QueueURL: cfg.SQSQueueURL,
		},
		Client:  rdb,
		Channel: api.LiveChannel,
		Logger:  logger,
	}
	relay := outbox.NewRelay(pool, publisher, cfg.OutboxPollInterval, cfg.OutboxBatchSize, logger)

	group, groupCtx := errgroup.WithContext(ctx)

	logger.Info("ingest listening", "addr", cfg.Addr)
	boot.ServeHTTP(groupCtx, group, server, cfg.ShutdownTimeout, logger)

	group.Go(func() error {
		return relay.Run(groupCtx)
	})

	// Runtime diagnostics on loopback, off unless PPROF_ADDR is set.
	// Convention: 127.0.0.1:6062 for this binary.
	group.Go(func() error { return debugx.Serve(groupCtx, cfg.PprofAddr, logger) })

	if err := group.Wait(); err != nil {
		return err
	}
	logger.Info("stopped cleanly")
	return nil
}
