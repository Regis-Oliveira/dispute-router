// Command worker decides what happens to disputes before their deadlines pass.
//
// The third process, and the one that makes the platform do something rather
// than only record things. It reads the deadline index out of Redis, takes a
// lock, re-reads the dispute from Postgres, applies the policy in
// internal/worker/rules.go, and writes the outcome.
package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"
	"golang.org/x/sync/errgroup"

	"github.com/regisoliveira/dispute-router/internal/awsx"
	"github.com/regisoliveira/dispute-router/internal/config"
	"github.com/regisoliveira/dispute-router/internal/debugx"
	"github.com/regisoliveira/dispute-router/internal/worker"
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

	// Names the process in every dispute_events row it writes, so an automated
	// decision can be traced back to the thing that made it.
	id, err := os.Hostname()
	if err != nil || id == "" {
		id = "worker"
	}

	workers := worker.NewPool(worker.Options{
		Store:             worker.NewStore(pool),
		Deadlines:         worker.NewDeadlines(rdb),
		Locks:             worker.NewLocks(rdb, cfg.WorkerLockTTL),
		Logger:            logger,
		Concurrency:       cfg.WorkerConcurrency,
		PollInterval:      cfg.WorkerPollInterval,
		BatchSize:         cfg.WorkerBatchSize,
		Lookahead:         cfg.WorkerLookahead,
		ReconcileInterval: cfg.WorkerReconcileInterval,
		ID:                id,
	})

	// The SQS consumer schedules a dispute the moment it arrives; the reconcile
	// pass inside the pool remains the guarantee that nothing is ever missed.
	awsCfg, err := awsx.Load(ctx, awsx.Config{
		Region:          cfg.AWSRegion,
		Endpoint:        cfg.AWSEndpoint,
		AccessKeyID:     cfg.AWSAccessKey,
		SecretAccessKey: cfg.AWSSecretKey,
	})
	if err != nil {
		return err
	}

	consumer := &worker.Consumer{
		Client:      awsx.SQS(awsCfg, cfg.AWSEndpoint),
		QueueURL:    cfg.SQSQueueURL,
		Deadlines:   worker.NewDeadlines(rdb),
		Logger:      logger,
		MaxMessages: int32(cfg.SQSMaxMessages),
		WaitTime:    int32(cfg.SQSWaitSeconds),
	}

	logger.Info("worker starting",
		"concurrency", cfg.WorkerConcurrency,
		"poll", cfg.WorkerPollInterval.String(),
		"lookahead", cfg.WorkerLookahead.String(),
		"reconcile", cfg.WorkerReconcileInterval.String(),
		"id", id)

	group, groupCtx := errgroup.WithContext(ctx)
	group.Go(func() error { return workers.Run(groupCtx) })
	group.Go(func() error { return consumer.Run(groupCtx) })
	// Runtime diagnostics on loopback, off unless PPROF_ADDR is set. This is
	// the process worth tracing: eight handlers waiting on Postgres and
	// Redis is what parallelism looks like here, and the execution trace
	// shows it per logical processor. Convention: 127.0.0.1:6061.
	group.Go(func() error { return debugx.Serve(groupCtx, cfg.PprofAddr, logger) })

	if err := group.Wait(); err != nil {
		return err
	}

	// Give the last in-flight decisions a moment to finish their writes.
	time.Sleep(200 * time.Millisecond)
	logger.Info("stopped cleanly")
	return nil
}
