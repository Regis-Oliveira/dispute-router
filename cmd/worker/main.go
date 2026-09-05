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

	"github.com/regisoliveira/dispute-router/internal/config"
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

	logger.Info("worker starting",
		"concurrency", cfg.WorkerConcurrency,
		"poll", cfg.WorkerPollInterval.String(),
		"lookahead", cfg.WorkerLookahead.String(),
		"reconcile", cfg.WorkerReconcileInterval.String(),
		"id", id)

	if err := workers.Run(ctx); err != nil {
		return err
	}

	// Give the last in-flight decisions a moment to finish their writes.
	time.Sleep(200 * time.Millisecond)
	logger.Info("stopped cleanly")
	return nil
}
