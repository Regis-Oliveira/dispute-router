// Package boot holds the startup and shutdown steps every binary repeats.
//
// It lives under cmd/internal so that Go itself restricts it to cmd/..., which
// is the point: these helpers encode how a *process* starts, and nothing in
// internal/ should be able to reach for them.
package boot

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"
	"golang.org/x/sync/errgroup"
)

// Logger builds the process logger at info level.
//
// The two shapes are a real per-binary choice, not a default and an exception.
// A long-running service logs JSON on stdout, where a collector reads it; a
// command run by a person logs text on stderr, so that its output stays its
// own - and for cmd/mcp stdout is the protocol, where a stray line corrupts
// the stream.
func Logger(json bool) *slog.Logger {
	options := &slog.HandlerOptions{Level: slog.LevelInfo}
	if json {
		return slog.New(slog.NewJSONHandler(os.Stdout, options))
	}
	return slog.New(slog.NewTextHandler(os.Stderr, options))
}

// Postgres opens the pool and proves it works before returning it.
//
// Failing at startup rather than on the first request makes a bad DSN a crash
// loop instead of a stream of 500s. The caller still owns the pool and should
// defer Close.
func Postgres(ctx context.Context, url string) (*pgxpool.Pool, error) {
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		return nil, fmt.Errorf("connect to postgres: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("ping postgres: %w", err)
	}
	return pool, nil
}

// Redis opens the client and proves it works before returning it, for the same
// reason Postgres does. The caller still owns the client and should defer
// Close.
func Redis(ctx context.Context, url string) (*redis.Client, error) {
	options, err := redis.ParseURL(url)
	if err != nil {
		return nil, fmt.Errorf("parse redis url: %w", err)
	}
	client := redis.NewClient(options)
	if err := client.Ping(ctx).Err(); err != nil {
		_ = client.Close()
		return nil, fmt.Errorf("ping redis: %w", err)
	}
	return client, nil
}

// ServeHTTP adds two goroutines to group: one that serves until the server is
// shut down, and one that shuts it down when ctx is cancelled, giving
// in-flight requests grace to finish.
//
// ctx is the group's context, so a failure anywhere else in the group also
// closes the listener.
func ServeHTTP(ctx context.Context, group *errgroup.Group, server *http.Server, grace time.Duration, logger *slog.Logger) {
	group.Go(func() error {
		if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			return err
		}
		return nil
	})

	group.Go(func() error {
		<-ctx.Done()
		// A fresh context: ctx is already cancelled, and Shutdown needs a live
		// one to give in-flight requests their grace period.
		shutdownCtx, cancel := context.WithTimeout(context.Background(), grace)
		defer cancel()
		logger.Info("shutting down", "grace", grace.String())
		return server.Shutdown(shutdownCtx)
	})
}
