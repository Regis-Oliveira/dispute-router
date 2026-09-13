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

	"golang.org/x/sync/errgroup"

	"github.com/regisoliveira/dispute-router/cmd/internal/boot"
	"github.com/regisoliveira/dispute-router/internal/awsx"
	"github.com/regisoliveira/dispute-router/internal/config"
	"github.com/regisoliveira/dispute-router/internal/debugx"
	"github.com/regisoliveira/dispute-router/internal/worker"
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

	// Names the process in every dispute_events row it writes, so an automated
	// decision can be traced back to the thing that made it.
	id, err := os.Hostname()
	if err != nil || id == "" {
		id = "worker"
	}

	deadlines := worker.NewDeadlines(rdb)
	workers := worker.NewPool(worker.Options{
		Store:             worker.NewStore(pool),
		Deadlines:         deadlines,
		Locks:             worker.NewLocks(rdb, cfg.Worker.LockTTL),
		Logger:            logger,
		Concurrency:       cfg.Worker.Concurrency,
		PollInterval:      cfg.Worker.PollInterval,
		BatchSize:         cfg.Worker.BatchSize,
		Lookahead:         cfg.Worker.Lookahead,
		ReconcileInterval: cfg.Worker.ReconcileInterval,
		ID:                id,
	})

	// The SQS consumer schedules a dispute the moment it arrives; the reconcile
	// pass inside the pool remains the guarantee that nothing is ever missed.
	awsCfg, err := awsx.Load(ctx, cfg.AWS.Config)
	if err != nil {
		return err
	}

	consumer := worker.NewConsumer(worker.ConsumerOptions{
		Client:      awsx.SQS(awsCfg),
		QueueURL:    cfg.AWS.QueueURL,
		Deadlines:   deadlines,
		Logger:      logger,
		MaxMessages: int32(cfg.AWS.MaxMessages),
		WaitTime:    int32(cfg.AWS.WaitSeconds),
	})

	logger.Info("worker starting",
		"concurrency", cfg.Worker.Concurrency,
		"poll", cfg.Worker.PollInterval.String(),
		"lookahead", cfg.Worker.Lookahead.String(),
		"reconcile", cfg.Worker.ReconcileInterval.String(),
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
	logger.Info("stopped cleanly")
	return nil
}
