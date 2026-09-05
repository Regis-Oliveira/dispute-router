package ingest

import (
	"context"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"
)

// Guard answers "have I already handled this event?" in microseconds.
//
// Redis is the fast path, not the source of truth. The unique index on
// webhook_events.idempotency_key is what actually guarantees an event is
// handled once; this only keeps the common case from touching Postgres at all.
// Flush Redis and the system stays correct - it just gets slower for a while.
type Guard struct {
	rdb *redis.Client
	ttl time.Duration
}

func NewGuard(rdb *redis.Client, ttl time.Duration) *Guard {
	return &Guard{rdb: rdb, ttl: ttl}
}

func (g *Guard) key(eventID string) string {
	return "ingest:seen:" + eventID
}

// Claim returns true if this caller is the first to see the event.
//
// SETNX is atomic, so two concurrent deliveries of the same event cannot both
// win: exactly one gets true and the other is told to stop.
func (g *Guard) Claim(ctx context.Context, eventID string) (bool, error) {
	ok, err := g.rdb.SetNX(ctx, g.key(eventID), "1", g.ttl).Result()
	if err != nil {
		return false, fmt.Errorf("claim idempotency key: %w", err)
	}
	return ok, nil
}

// Release drops a claim whose work did not finish.
//
// This is the part that is easy to leave out and expensive to debug. Claim the
// key, then fail to write to Postgres, and without this the sender's retry is
// answered "already handled" for the next 24 hours - the event is lost, and
// every log line says it succeeded.
func (g *Guard) Release(ctx context.Context, eventID string) error {
	if err := g.rdb.Del(ctx, g.key(eventID)).Err(); err != nil {
		return fmt.Errorf("release idempotency key: %w", err)
	}
	return nil
}
