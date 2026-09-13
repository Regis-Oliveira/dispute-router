package ingest

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/regisoliveira/dispute-router/internal/dispute"
	"github.com/regisoliveira/dispute-router/internal/events"
	"github.com/regisoliveira/dispute-router/internal/ledger"
	"github.com/regisoliveira/dispute-router/internal/outbox"
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

// Store is the ingest write path into Postgres.
type Store struct {
	pool *pgxpool.Pool
}

// NewStore wraps a connection pool.
func NewStore(pool *pgxpool.Pool) *Store {
	return &Store{pool: pool}
}

// Merchant deliberately no longer carries the signing key.
//
// It used to, which meant every code path that wanted a merchant's currency
// also held its credential, and every log line that dumped the struct leaked
// it. Secrets are resolved separately, by the one component that needs them.
type Merchant struct {
	ID                     int64
	ExternalID             string
	Currency               string
	AutoRefundCeilingMinor *int64
}

// MerchantByExternalID looks up the merchant a webhook claims to be from.
func (s *Store) MerchantByExternalID(ctx context.Context, externalID string) (Merchant, error) {
	var m Merchant
	err := s.pool.QueryRow(ctx, `
		SELECT id, external_id, currency, auto_refund_ceiling_minor
		  FROM merchants
		 WHERE external_id = $1`, externalID,
	).Scan(&m.ID, &m.ExternalID, &m.Currency, &m.AutoRefundCeilingMinor)

	if errors.Is(err, pgx.ErrNoRows) {
		return Merchant{}, ErrUnknownMerchant
	}
	if err != nil {
		return Merchant{}, fmt.Errorf("load merchant %s: %w", externalID, err)
	}
	return m, nil
}

// recordResult reports what record or recordRuling actually did.
type recordResult struct {
	WebhookEventID int64
	DisputeID      int64
	// Duplicate is true when Postgres, not Redis, caught the repeat - which
	// happens whenever the Redis key has expired or been flushed.
	Duplicate bool
}

// record durably stores one delivery.
//
// Everything below happens in a single transaction, which is the whole point:
// the raw payload, the dispute it produced, its first audit event and the
// outbox message telling the rest of the system about it either all exist or
// none of them do. There is no window where a dispute exists but nothing
// downstream will ever hear about it.
func (s *Store) record(
	ctx context.Context,
	merchant Merchant,
	raw []byte,
	signature string,
	event DisputeWebhook,
) (recordResult, error) {
	var (
		result recordResult
		// outcome is how a committed failure leaves the transaction. The body
		// below returns nil on those paths so pgx.BeginFunc commits what was
		// recorded, and the error the caller sees travels out here instead.
		outcome error
	)

	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		webhookEventID, duplicate, err := insertDelivery(ctx, tx, merchant.ID, event.Type, event.ID, raw, signature)
		if err != nil {
			return err
		}
		if duplicate {
			result = recordResult{Duplicate: true}
			return nil
		}

		var transactionID int64
		err = tx.QueryRow(ctx, `
			SELECT id FROM transactions WHERE merchant_id = $1 AND external_id = $2`,
			merchant.ID, event.Data.TransactionID,
		).Scan(&transactionID)

		if errors.Is(err, pgx.ErrNoRows) {
			// Commit the failure rather than discarding it. Rolling back here would
			// erase the evidence and let the same bad event arrive again forever.
			if err := markFailed(ctx, tx, webhookEventID, "unknown transaction "+event.Data.TransactionID); err != nil {
				return err
			}
			result = recordResult{WebhookEventID: webhookEventID}
			outcome = ErrUnknownTransaction
			return nil
		}
		if err != nil {
			return fmt.Errorf("load transaction: %w", err)
		}

		var disputeID int64
		err = tx.QueryRow(ctx, `
			INSERT INTO disputes (
				merchant_id, transaction_id, external_id, kind, card_network, reason_code,
				amount_minor, currency, state, deadline_at, opened_at, cardholder_claim
			)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12)
			ON CONFLICT (merchant_id, external_id) DO NOTHING
			RETURNING id`,
			merchant.ID, transactionID, event.Data.DisputeID, event.Data.Kind,
			event.Data.CardNetwork, event.Data.ReasonCode, event.Data.AmountMinor,
			event.Data.Currency, dispute.StateReceived, event.Data.RespondBy,
			event.Data.OpenedAt, event.Data.CardholderClaim,
		).Scan(&disputeID)

		if errors.Is(err, pgx.ErrNoRows) {
			// A second distinct event describing a dispute already on file. The
			// event log keeps the delivery; the dispute is left as it is.
			if err := markProcessed(ctx, tx, webhookEventID); err != nil {
				return err
			}
			result = recordResult{WebhookEventID: webhookEventID, Duplicate: true}
			return nil
		}
		if err != nil {
			return fmt.Errorf("insert dispute: %w", err)
		}

		// A chargeback is a clawback that has already happened: the acquirer took
		// the money when the network filed it, and the outcome is weeks away. An
		// alert is only a warning, so nothing moves.
		if dispute.Kind(event.Data.Kind) == dispute.KindChargeback {
			if err := ledger.Hold(ctx, tx, ledger.Entry{
				DisputeID:   disputeID,
				MerchantID:  merchant.ID,
				AmountMinor: event.Data.AmountMinor,
				Currency:    event.Data.Currency,
				At:          event.Data.OpenedAt,
			}, event.Data.ReasonCode); err != nil {
				return err
			}
		}

		// occurred_at is the processor's own timestamp, not the moment this row
		// was written: the audit trail is read in the order things happened.
		if err := events.Record(ctx, tx, events.Event{
			DisputeID: disputeID,
			ToState:   dispute.StateReceived,
			Actor:     "system",
			Detail: map[string]any{
				"source":           "webhook",
				"webhook_event_id": webhookEventID,
				"reason_code":      event.Data.ReasonCode,
				"kind":             event.Data.Kind,
			},
			OccurredAt: event.Data.OpenedAt,
		}); err != nil {
			return err
		}

		if err := outbox.Insert(ctx, tx, outbox.Entry{
			AggregateType: "dispute",
			AggregateID:   disputeID,
			EventType:     "dispute.received",
			Payload: map[string]any{
				"dispute_id":   disputeID,
				"merchant_id":  merchant.ID,
				"kind":         event.Data.Kind,
				"amount_minor": event.Data.AmountMinor,
				"currency":     event.Data.Currency,
				"deadline_at":  event.Data.RespondBy,
			},
		}); err != nil {
			return err
		}

		if err := markProcessed(ctx, tx, webhookEventID); err != nil {
			return err
		}

		result = recordResult{WebhookEventID: webhookEventID, DisputeID: disputeID}
		return nil
	})
	if err != nil {
		return recordResult{}, err
	}
	return result, outcome
}

// insertDelivery lands the raw payload before anything is interpreted.
//
// First and unconditionally: if parsing or linking fails after this point the
// event is still on disk, replayable, and arguable - which is the difference
// between a support ticket and a shrug.
//
// A true duplicate means ON CONFLICT DO NOTHING returned no row, so the
// idempotency key has been seen before. Not an error: it is the durable
// idempotency check doing its job, behind the Redis one that usually answers
// first.
func insertDelivery(
	ctx context.Context, tx pgx.Tx,
	merchantID int64, eventType, idempotencyKey string,
	raw []byte, signature string,
) (id int64, duplicate bool, err error) {
	err = tx.QueryRow(ctx, `
		INSERT INTO webhook_events (merchant_id, source, event_type, idempotency_key, payload, signature, status)
		VALUES ($1, 'processor', $2, $3, $4::jsonb, $5, 'pending')
		ON CONFLICT (idempotency_key) DO NOTHING
		RETURNING id`,
		merchantID, eventType, idempotencyKey, string(raw), signature,
	).Scan(&id)

	if errors.Is(err, pgx.ErrNoRows) {
		return 0, true, nil
	}
	if err != nil {
		return 0, false, fmt.Errorf("insert webhook_event: %w", err)
	}
	return id, false, nil
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

// ErrNotRepresented means the ruling arrived for a dispute that was never
// argued, or that has already been ruled on.
var ErrNotRepresented = errors.New("dispute is not awaiting a ruling")

// recordRuling applies the network's verdict.
//
// This is what closes the lifecycle. Until it existed, "represented" was
// terminal: the merchant submitted evidence and the system had no way to learn
// what came of it, so the ledger could never show the outcome of a fight it had
// started.
func (s *Store) recordRuling(
	ctx context.Context,
	merchant Merchant,
	raw []byte,
	signature string,
	event RulingWebhook,
) (recordResult, error) {
	var (
		result recordResult
		// Same shape as record: a failure that is nevertheless committed leaves
		// through this variable rather than out of the transaction body.
		outcome error
	)

	// The network's verdict and the dispute's two terminal states are the same
	// two words. Validate has already refused anything that is not one of them,
	// so this conversion cannot widen what reaches the CHECK constraint.
	ruled := dispute.State(event.Data.Outcome)

	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		webhookEventID, duplicate, err := insertDelivery(ctx, tx, merchant.ID, event.Type, event.ID, raw, signature)
		if err != nil {
			return err
		}
		if duplicate {
			result = recordResult{Duplicate: true}
			return nil
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
			 WHERE merchant_id = $1 AND external_id = $2 AND state = $5
			RETURNING id, transaction_id, amount_minor, currency`,
			merchant.ID, event.Data.DisputeID, ruled, event.Data.DecidedAt,
			dispute.StateRepresented,
		).Scan(&disputeID, &transactionID, &amountMinor, &currency)

		if errors.Is(err, pgx.ErrNoRows) {
			// Recorded as failed rather than rolled back: a ruling for a dispute
			// that is not awaiting one is worth being able to look at later.
			if err := markFailed(ctx, tx, webhookEventID,
				"no represented dispute "+event.Data.DisputeID); err != nil {
				return err
			}
			result = recordResult{WebhookEventID: webhookEventID}
			outcome = ErrNotRepresented
			return nil
		}
		if err != nil {
			return fmt.Errorf("apply ruling: %w", err)
		}

		if err := events.Record(ctx, tx, events.Event{
			DisputeID: disputeID,
			FromState: dispute.StateRepresented,
			ToState:   ruled,
			Actor:     "network",
			Detail: map[string]any{
				"source":           "webhook",
				"webhook_event_id": webhookEventID,
				"outcome":          event.Data.Outcome,
				"note":             event.Data.Note,
			},
			OccurredAt: event.Data.DecidedAt,
		}); err != nil {
			return err
		}

		// Both outcomes move money now, because the funds were held when the
		// chargeback arrived. A win releases the hold back to the merchant; a loss
		// sends it to the issuer and charges the fee.
		entry := ledger.Entry{
			DisputeID:   disputeID,
			MerchantID:  merchant.ID,
			AmountMinor: amountMinor,
			Currency:    currency,
			At:          event.Data.DecidedAt,
		}
		if ruled == dispute.StateLost {
			err = ledger.SettleLoss(ctx, tx, entry, "representment lost")
		} else {
			err = ledger.ReleaseHold(ctx, tx, entry)
		}
		if err != nil {
			return err
		}

		if err := outbox.Insert(ctx, tx, outbox.Entry{
			AggregateType: "dispute",
			AggregateID:   disputeID,
			EventType:     "dispute." + event.Data.Outcome,
			Payload: map[string]any{
				"dispute_id":   disputeID,
				"merchant_id":  merchant.ID,
				"outcome":      event.Data.Outcome,
				"amount_minor": amountMinor,
				"currency":     currency,
			},
		}); err != nil {
			return err
		}

		if err := markProcessed(ctx, tx, webhookEventID); err != nil {
			return err
		}

		result = recordResult{WebhookEventID: webhookEventID, DisputeID: disputeID}
		return nil
	})
	if err != nil {
		return recordResult{}, err
	}
	return result, outcome
}
