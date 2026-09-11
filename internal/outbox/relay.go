// Package outbox drains committed outbox rows into the message queue.
package outbox

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"
)

// Publisher is whatever the messages go to. Phase 1 logs them; Phase 4 swaps in
// SQS without the relay changing.
type Publisher interface {
	Publish(ctx context.Context, msg Message) error
}

type Message struct {
	ID            int64
	AggregateType string
	AggregateID   int64
	EventType     string
	Payload       json.RawMessage
}

type Relay struct {
	pool      *pgxpool.Pool
	publisher Publisher
	interval  time.Duration
	batchSize int
	logger    *slog.Logger
}

func NewRelay(pool *pgxpool.Pool, publisher Publisher, interval time.Duration, batchSize int, logger *slog.Logger) *Relay {
	return &Relay{
		pool:      pool,
		publisher: publisher,
		interval:  interval,
		batchSize: batchSize,
		logger:    logger,
	}
}

// Run polls until the context is cancelled.
func (r *Relay) Run(ctx context.Context) error {
	ticker := time.NewTicker(r.interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			r.logger.InfoContext(ctx, "outbox relay stopping")
			return nil
		case <-ticker.C:
			for {
				n, err := r.drainOnce(ctx)
				if err != nil {
					// A failed batch stays unpublished and is retried on the
					// next tick. Nothing is lost; the queue just runs behind.
					r.logger.ErrorContext(ctx, "outbox drain failed", "error", err)
					break
				}
				// Keep going while there is a full batch waiting, so a backlog
				// clears at full speed instead of one batch per tick.
				if n < r.batchSize {
					break
				}
			}
		}
	}
}

// drainOnce publishes at most one batch.
//
// FOR UPDATE SKIP LOCKED is what lets several relay instances run at once: each
// takes rows nobody else has locked instead of queueing behind them.
//
// The order is publish first, then mark published, then commit. If the process
// dies between publishing and committing, the message is delivered twice - and
// at-least-once is the right trade, because the alternative (mark, then
// publish) loses messages outright on the same crash. Consumers must be
// idempotent, which is exactly what the ingest path already demonstrates.
func (r *Relay) drainOnce(ctx context.Context) (int, error) {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return 0, fmt.Errorf("begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	rows, err := tx.Query(ctx, `
		SELECT id, aggregate_type, aggregate_id, event_type, payload
		  FROM outbox
		 WHERE published_at IS NULL
		 ORDER BY id
		 LIMIT $1
		   FOR UPDATE SKIP LOCKED`, r.batchSize)
	if err != nil {
		return 0, fmt.Errorf("select outbox: %w", err)
	}

	var batch []Message
	for rows.Next() {
		var msg Message
		if err := rows.Scan(&msg.ID, &msg.AggregateType, &msg.AggregateID, &msg.EventType, &msg.Payload); err != nil {
			rows.Close()
			return 0, fmt.Errorf("scan outbox row: %w", err)
		}
		batch = append(batch, msg)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, fmt.Errorf("read outbox rows: %w", err)
	}
	if len(batch) == 0 {
		return 0, nil
	}

	ids := make([]int64, 0, len(batch))
	var publishErr error
	for _, msg := range batch {
		if err := r.publisher.Publish(ctx, msg); err != nil {
			// Stop at the first failure and commit the ones that made it, so a
			// broken message does not block the ones behind it forever.
			publishErr = err
			break
		}
		ids = append(ids, msg.ID)
	}

	if len(ids) == 0 {
		// The cause is wrapped rather than summarised. "publisher rejected
		// every message" describes the symptom and hides the one thing needed
		// to fix it, which cost a debugging round trip to learn.
		return 0, fmt.Errorf("publisher rejected every message in the batch: %w", publishErr)
	}

	if _, err := tx.Exec(ctx, `UPDATE outbox SET published_at = now() WHERE id = ANY($1)`, ids); err != nil {
		return 0, fmt.Errorf("mark published: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, fmt.Errorf("commit: %w", err)
	}

	return len(ids), nil
}

// Live wraps a Publisher and additionally broadcasts each message on a Redis
// pub/sub channel so the dashboard can show arrivals as they happen.
//
// The broadcast is deliberately best-effort and deliberately *after* the real
// publish: pub/sub has no queue and no retention, so a message sent while
// nobody is subscribed is simply gone. That is fine for a live feed and would
// be a data-loss bug for delivery, which is why a failed broadcast never fails
// the batch and never stops a row being marked published.
type Live struct {
	Next    Publisher
	Client  *redis.Client
	Channel string
	Logger  *slog.Logger
}

func (p Live) Publish(ctx context.Context, msg Message) error {
	if err := p.Next.Publish(ctx, msg); err != nil {
		return err
	}

	broadcast, err := json.Marshal(struct {
		OutboxID    int64           `json:"outbox_id"`
		EventType   string          `json:"event_type"`
		AggregateID int64           `json:"aggregate_id"`
		Payload     json.RawMessage `json:"payload"`
		At          time.Time       `json:"at"`
	}{msg.ID, msg.EventType, msg.AggregateID, msg.Payload, time.Now().UTC()})
	if err != nil {
		p.Logger.WarnContext(ctx, "live broadcast encode failed", "error", err, "outbox_id", msg.ID)
		return nil
	}

	if err := p.Client.Publish(ctx, p.Channel, broadcast).Err(); err != nil {
		p.Logger.WarnContext(ctx, "live broadcast failed", "error", err, "outbox_id", msg.ID)
	}
	return nil
}
