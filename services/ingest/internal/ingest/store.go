package ingest

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

var (
	// ErrUnknownMerchant is deliberately indistinguishable from a signature
	// failure at the HTTP boundary. Answering "no such merchant" differently
	// from "bad signature" turns the endpoint into a merchant-id oracle.
	ErrUnknownMerchant = errors.New("unknown merchant")

	// ErrUnknownTransaction means the event is well-formed and correctly signed
	// but points at a charge this platform has never seen. Worth a distinct
	// status: the sender should stop retrying, and somebody should look.
	ErrUnknownTransaction = errors.New("unknown transaction")
)

type Store struct {
	pool *pgxpool.Pool
}

func NewStore(pool *pgxpool.Pool) *Store {
	return &Store{pool: pool}
}

type Merchant struct {
	ID                     int64
	ExternalID             string
	WebhookSecret          string
	Currency               string
	AutoRefundCeilingMinor *int64
}

func (s *Store) MerchantByExternalID(ctx context.Context, externalID string) (Merchant, error) {
	var m Merchant
	err := s.pool.QueryRow(ctx, `
		SELECT id, external_id, webhook_secret, currency, auto_refund_ceiling_minor
		  FROM merchants
		 WHERE external_id = $1`, externalID,
	).Scan(&m.ID, &m.ExternalID, &m.WebhookSecret, &m.Currency, &m.AutoRefundCeilingMinor)

	if errors.Is(err, pgx.ErrNoRows) {
		return Merchant{}, ErrUnknownMerchant
	}
	if err != nil {
		return Merchant{}, fmt.Errorf("load merchant %s: %w", externalID, err)
	}
	return m, nil
}

// RecordResult reports what a Record call actually did.
type RecordResult struct {
	WebhookEventID int64
	DisputeID      int64
	// Duplicate is true when Postgres, not Redis, caught the repeat - which
	// happens whenever the Redis key has expired or been flushed.
	Duplicate bool
}

// Record durably stores one delivery.
//
// Everything below happens in a single transaction, which is the whole point:
// the raw payload, the dispute it produced, its first audit event and the
// outbox message telling the rest of the system about it either all exist or
// none of them do. There is no window where a dispute exists but nothing
// downstream will ever hear about it.
func (s *Store) Record(
	ctx context.Context,
	merchant Merchant,
	raw []byte,
	signature string,
	event DisputeWebhook,
) (RecordResult, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return RecordResult{}, fmt.Errorf("begin: %w", err)
	}
	// Rollback after a successful Commit is a no-op, so this is safe to defer
	// unconditionally and is the only thing that guarantees the transaction is
	// released on every early return below.
	defer func() { _ = tx.Rollback(ctx) }()

	// The raw payload lands first and unconditionally. If parsing or linking
	// fails after this point the event is still on disk, replayable, and
	// arguable - which is the difference between a support ticket and a shrug.
	var webhookEventID int64
	err = tx.QueryRow(ctx, `
		INSERT INTO webhook_events (merchant_id, source, event_type, idempotency_key, payload, signature, status)
		VALUES ($1, 'processor', $2, $3, $4::jsonb, $5, 'pending')
		ON CONFLICT (idempotency_key) DO NOTHING
		RETURNING id`,
		merchant.ID, event.Type, event.ID, string(raw), signature,
	).Scan(&webhookEventID)

	if errors.Is(err, pgx.ErrNoRows) {
		// ON CONFLICT DO NOTHING returned nothing, so this key already exists.
		// Not an error: it is the durable idempotency check doing its job.
		return RecordResult{Duplicate: true}, nil
	}
	if err != nil {
		return RecordResult{}, fmt.Errorf("insert webhook_event: %w", err)
	}

	var transactionID int64
	err = tx.QueryRow(ctx, `
		SELECT id FROM transactions WHERE merchant_id = $1 AND external_id = $2`,
		merchant.ID, event.Data.TransactionID,
	).Scan(&transactionID)

	if errors.Is(err, pgx.ErrNoRows) {
		// Commit the failure rather than discarding it. Rolling back here would
		// erase the evidence and let the same bad event arrive again forever.
		if markErr := markFailed(ctx, tx, webhookEventID, "unknown transaction "+event.Data.TransactionID); markErr != nil {
			return RecordResult{}, markErr
		}
		if commitErr := tx.Commit(ctx); commitErr != nil {
			return RecordResult{}, fmt.Errorf("commit failed webhook_event: %w", commitErr)
		}
		return RecordResult{WebhookEventID: webhookEventID}, ErrUnknownTransaction
	}
	if err != nil {
		return RecordResult{}, fmt.Errorf("load transaction: %w", err)
	}

	var disputeID int64
	err = tx.QueryRow(ctx, `
		INSERT INTO disputes (
			merchant_id, transaction_id, external_id, kind, card_network, reason_code,
			amount_minor, currency, state, deadline_at, opened_at
		)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, 'received', $9, $10)
		ON CONFLICT (merchant_id, external_id) DO NOTHING
		RETURNING id`,
		merchant.ID, transactionID, event.Data.DisputeID, event.Data.Kind,
		event.Data.CardNetwork, event.Data.ReasonCode, event.Data.AmountMinor,
		event.Data.Currency, event.Data.RespondBy, event.Data.OpenedAt,
	).Scan(&disputeID)

	if errors.Is(err, pgx.ErrNoRows) {
		// A second distinct event describing a dispute already on file. The
		// event log keeps the delivery; the dispute is left as it is.
		if markErr := markProcessed(ctx, tx, webhookEventID); markErr != nil {
			return RecordResult{}, markErr
		}
		if commitErr := tx.Commit(ctx); commitErr != nil {
			return RecordResult{}, fmt.Errorf("commit: %w", commitErr)
		}
		return RecordResult{WebhookEventID: webhookEventID, Duplicate: true}, nil
	}
	if err != nil {
		return RecordResult{}, fmt.Errorf("insert dispute: %w", err)
	}

	detail, err := json.Marshal(map[string]any{
		"source":           "webhook",
		"webhook_event_id": webhookEventID,
		"reason_code":      event.Data.ReasonCode,
		"kind":             event.Data.Kind,
	})
	if err != nil {
		return RecordResult{}, fmt.Errorf("marshal dispute event detail: %w", err)
	}

	if _, err := tx.Exec(ctx, `
		INSERT INTO dispute_events (dispute_id, from_state, to_state, actor, detail, occurred_at)
		VALUES ($1, NULL, 'received', 'system', $2::jsonb, $3)`,
		disputeID, string(detail), event.Data.OpenedAt,
	); err != nil {
		return RecordResult{}, fmt.Errorf("insert dispute_event: %w", err)
	}

	// The outbox message commits with the dispute. A publish to SQS cannot be
	// part of this transaction, so the relay reads committed rows instead -
	// which is what makes "the dispute exists but the queue never heard about
	// it" impossible.
	outboxPayload, err := json.Marshal(map[string]any{
		"dispute_id":   disputeID,
		"merchant_id":  merchant.ID,
		"kind":         event.Data.Kind,
		"amount_minor": event.Data.AmountMinor,
		"currency":     event.Data.Currency,
		"deadline_at":  event.Data.RespondBy,
	})
	if err != nil {
		return RecordResult{}, fmt.Errorf("marshal outbox payload: %w", err)
	}

	if _, err := tx.Exec(ctx, `
		INSERT INTO outbox (aggregate_type, aggregate_id, event_type, payload)
		VALUES ('dispute', $1, 'dispute.received', $2::jsonb)`,
		disputeID, string(outboxPayload),
	); err != nil {
		return RecordResult{}, fmt.Errorf("insert outbox: %w", err)
	}

	if err := markProcessed(ctx, tx, webhookEventID); err != nil {
		return RecordResult{}, err
	}

	if err := tx.Commit(ctx); err != nil {
		return RecordResult{}, fmt.Errorf("commit: %w", err)
	}

	return RecordResult{WebhookEventID: webhookEventID, DisputeID: disputeID}, nil
}

func markProcessed(ctx context.Context, tx pgx.Tx, id int64) error {
	if _, err := tx.Exec(ctx, `
		UPDATE webhook_events
		   SET status = 'processed', processed_at = now(), attempts = attempts + 1
		 WHERE id = $1`, id,
	); err != nil {
		return fmt.Errorf("mark webhook_event processed: %w", err)
	}
	return nil
}

func markFailed(ctx context.Context, tx pgx.Tx, id int64, reason string) error {
	if _, err := tx.Exec(ctx, `
		UPDATE webhook_events
		   SET status = 'failed', last_error = $2, attempts = attempts + 1, processed_at = now()
		 WHERE id = $1`, id, reason,
	); err != nil {
		return fmt.Errorf("mark webhook_event failed: %w", err)
	}
	return nil
}

// Ping is the readiness probe's database half.
func (s *Store) Ping(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	return s.pool.Ping(ctx)
}
