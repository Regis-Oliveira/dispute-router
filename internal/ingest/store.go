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

// chargebackFeeMinor is what the network charges when a representment is lost,
// on top of the disputed amount.
const chargebackFeeMinor int64 = 1500

// ErrNotRepresented means the ruling arrived for a dispute that was never
// argued, or that has already been ruled on.
var ErrNotRepresented = errors.New("dispute is not awaiting a ruling")

// RecordRuling applies the network's verdict.
//
// This is what closes the lifecycle. Until it existed, "represented" was
// terminal: the merchant submitted evidence and the system had no way to learn
// what came of it, so the ledger could never show the outcome of a fight it had
// started.
func (s *Store) RecordRuling(
	ctx context.Context,
	merchant Merchant,
	raw []byte,
	signature string,
	event RulingWebhook,
) (RecordResult, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return RecordResult{}, fmt.Errorf("begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var webhookEventID int64
	err = tx.QueryRow(ctx, `
		INSERT INTO webhook_events (merchant_id, source, event_type, idempotency_key, payload, signature, status)
		VALUES ($1, 'processor', $2, $3, $4::jsonb, $5, 'pending')
		ON CONFLICT (idempotency_key) DO NOTHING
		RETURNING id`,
		merchant.ID, event.Type, event.ID, string(raw), signature,
	).Scan(&webhookEventID)

	if errors.Is(err, pgx.ErrNoRows) {
		return RecordResult{Duplicate: true}, nil
	}
	if err != nil {
		return RecordResult{}, fmt.Errorf("insert webhook_event: %w", err)
	}

	// Only a represented dispute can be ruled on, and the state test is part of
	// the UPDATE rather than a read followed by a write: two rulings arriving
	// at once would both pass a separate check and both post a loss.
	var (
		disputeID     int64
		transactionID int64
		amountMinor   int64
		currency      string
	)
	err = tx.QueryRow(ctx, `
		UPDATE disputes
		   SET state = $3,
		       resolved_at = $4,
		       version = version + 1
		 WHERE merchant_id = $1 AND external_id = $2 AND state = 'represented'
		RETURNING id, transaction_id, amount_minor, currency`,
		merchant.ID, event.Data.DisputeID, event.Data.Outcome, event.Data.DecidedAt,
	).Scan(&disputeID, &transactionID, &amountMinor, &currency)

	if errors.Is(err, pgx.ErrNoRows) {
		// Recorded as failed rather than rolled back: a ruling for a dispute
		// that is not awaiting one is worth being able to look at later.
		if markErr := markFailed(ctx, tx, webhookEventID,
			"no represented dispute "+event.Data.DisputeID); markErr != nil {
			return RecordResult{}, markErr
		}
		if commitErr := tx.Commit(ctx); commitErr != nil {
			return RecordResult{}, fmt.Errorf("commit failed webhook_event: %w", commitErr)
		}
		return RecordResult{WebhookEventID: webhookEventID}, ErrNotRepresented
	}
	if err != nil {
		return RecordResult{}, fmt.Errorf("apply ruling: %w", err)
	}

	detail, err := json.Marshal(map[string]any{
		"source":           "webhook",
		"webhook_event_id": webhookEventID,
		"outcome":          event.Data.Outcome,
		"note":             event.Data.Note,
	})
	if err != nil {
		return RecordResult{}, fmt.Errorf("marshal ruling detail: %w", err)
	}

	if _, err := tx.Exec(ctx, `
		INSERT INTO dispute_events (dispute_id, from_state, to_state, actor, detail, occurred_at)
		VALUES ($1, 'represented', $2, 'network', $3::jsonb, $4)`,
		disputeID, event.Data.Outcome, string(detail), event.Data.DecidedAt,
	); err != nil {
		return RecordResult{}, fmt.Errorf("insert dispute_event: %w", err)
	}

	if event.Data.Outcome == "lost" {
		if err := postChargebackLoss(ctx, tx, disputeID, merchant.ID, amountMinor, currency,
			event.Data.DecidedAt); err != nil {
			return RecordResult{}, err
		}
	}
	// A win moves no money here. In this model nothing was deducted when the
	// chargeback arrived, so there is nothing to give back - a real platform
	// holds the funds on arrival and releases them on a win, and that provisional
	// hold is the honest next thing to build.

	payload, err := json.Marshal(map[string]any{
		"dispute_id":   disputeID,
		"merchant_id":  merchant.ID,
		"outcome":      event.Data.Outcome,
		"amount_minor": amountMinor,
		"currency":     currency,
	})
	if err != nil {
		return RecordResult{}, fmt.Errorf("marshal outbox payload: %w", err)
	}

	if _, err := tx.Exec(ctx, `
		INSERT INTO outbox (aggregate_type, aggregate_id, event_type, payload)
		VALUES ('dispute', $1, $2, $3::jsonb)`,
		disputeID, "dispute."+event.Data.Outcome, string(payload),
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

// postChargebackLoss writes the three-legged entry a lost representment costs:
// the merchant loses the sale and the network's fee, so the debit is larger
// than the credit that returns the money.
func postChargebackLoss(
	ctx context.Context, tx pgx.Tx,
	disputeID, merchantID, amountMinor int64, currency string, occurredAt time.Time,
) error {
	var ledgerTxID int64
	err := tx.QueryRow(ctx, `
		INSERT INTO ledger_transactions (external_ref, kind, currency, description, occurred_at, metadata)
		VALUES ($1, 'chargeback', $2, 'representment lost', $3, $4::jsonb)
		RETURNING id`,
		fmt.Sprintf("dispute:%d:chargeback", disputeID),
		currency, occurredAt,
		fmt.Sprintf(`{"fee_minor":%d}`, chargebackFeeMinor),
	).Scan(&ledgerTxID)
	if err != nil {
		return fmt.Errorf("post chargeback for dispute %d: %w", disputeID, err)
	}

	// Every parameter cast: in an INSERT ... SELECT across a UNION ALL, Postgres
	// does not infer a parameter's type from the target column and an untyped
	// one defaults to text.
	if _, err := tx.Exec(ctx, `
		INSERT INTO ledger_entries (ledger_transaction_id, account_id, direction, amount_minor, currency)
		SELECT $1::bigint, a.id, 'debit', $2::bigint + $3::bigint, $4::char(3)
		  FROM ledger_accounts a
		 WHERE a.merchant_id = $5::bigint AND a.kind = 'merchant_balance' AND a.currency = $4::char(3)
		UNION ALL
		SELECT $1::bigint, a.id, 'credit', $2::bigint, $4::char(3)
		  FROM ledger_accounts a
		 WHERE a.merchant_id IS NULL AND a.kind = 'settlement_clearing' AND a.currency = $4::char(3)
		UNION ALL
		SELECT $1::bigint, a.id, 'credit', $3::bigint, $4::char(3)
		  FROM ledger_accounts a
		 WHERE a.merchant_id IS NULL AND a.kind = 'fee_revenue' AND a.currency = $4::char(3)`,
		ledgerTxID, amountMinor, chargebackFeeMinor, currency, merchantID,
	); err != nil {
		return fmt.Errorf("post chargeback entries for dispute %d: %w", disputeID, err)
	}
	return nil
}
