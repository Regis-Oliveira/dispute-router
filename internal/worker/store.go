package worker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ErrStaleCandidate means the dispute changed between being read and being
// written. Not a failure: the version check did its job.
var ErrStaleCandidate = errors.New("dispute changed underneath this worker")

// chargebackFeeMinor is what the network charges the merchant when a
// chargeback lands, on top of the disputed amount.
const chargebackFeeMinor int64 = 1500

type Store struct{ pool *pgxpool.Pool }

func NewStore(pool *pgxpool.Pool) *Store { return &Store{pool: pool} }

func (s *Store) Ping(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	return s.pool.Ping(ctx)
}

// loaded is a Candidate plus the bookkeeping the write path needs.
type loaded struct {
	Candidate
	MerchantID int64
	Version    int32
}

// Load reads a dispute at claim time.
//
// Deliberately re-read rather than carried through the queue: a dispute
// scheduled an hour ago may already have been resolved by a human, and acting
// on the version that was scheduled would undo their work.
func (s *Store) Load(ctx context.Context, disputeID int64) (loaded, error) {
	var l loaded
	err := s.pool.QueryRow(ctx, `
		SELECT d.id, d.merchant_id, d.kind, d.state, d.reason_code,
		       d.amount_minor, d.currency, d.deadline_at,
		       d.resolved_at IS NOT NULL, d.version,
		       m.auto_refund_ceiling_minor,
		       t.amount_minor - t.refunded_minor
		  FROM disputes d
		  JOIN merchants m    ON m.id = d.merchant_id
		  JOIN transactions t ON t.id = d.transaction_id
		 WHERE d.id = $1`, disputeID,
	).Scan(
		&l.ID, &l.MerchantID, &l.Kind, &l.State, &l.ReasonCode,
		&l.AmountMinor, &l.Currency, &l.DeadlineAt,
		&l.Resolved, &l.Version,
		&l.AutoRefundCeilingMinor,
		&l.RefundableRemainingMinor,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return loaded{}, pgx.ErrNoRows
	}
	if err != nil {
		return loaded{}, fmt.Errorf("load dispute %d: %w", disputeID, err)
	}
	return l, nil
}

// OpenDeadlines returns every dispute still on the clock.
//
// This is what makes the Redis index disposable: the answer to "what is due?"
// is always recoverable from here. Reads from the partial index on
// (deadline_at) WHERE state IN ('received','resolving'), so it stays cheap
// however much resolved history piles up behind it.
func (s *Store) OpenDeadlines(ctx context.Context) (map[int64]time.Time, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id, deadline_at
		  FROM disputes
		 WHERE state IN ('received','resolving')`)
	if err != nil {
		return nil, fmt.Errorf("load open deadlines: %w", err)
	}
	defer rows.Close()

	out := make(map[int64]time.Time)
	for rows.Next() {
		var id int64
		var deadline time.Time
		if err := rows.Scan(&id, &deadline); err != nil {
			return nil, fmt.Errorf("scan open deadline: %w", err)
		}
		out[id] = deadline
	}
	return out, rows.Err()
}

// Apply writes a decision.
//
// One transaction: the state change, its audit event, any money it moved, and
// the outbox message telling the rest of the system. A dispute cannot end up
// refunded with no ledger entry, or refunded with nothing downstream ever
// hearing about it.
func (s *Store) Apply(ctx context.Context, l loaded, decision Decision, workerID string) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if err := applyTx(ctx, tx, l, decision, workerID); err != nil {
		return err
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit: %w", err)
	}
	return nil
}

// applyTx is Apply's body, separated from the transaction that wraps it.
//
// Not an abstraction for its own sake: it is what lets a test run the real
// write path against a real database inside a transaction it then rolls back,
// so the SQL is exercised without a test leaving refunds behind in the ledger.
// The refund bug this split was added for - an uncast parameter defaulting to
// text - was invisible to every test that did not actually execute the insert.
func applyTx(ctx context.Context, tx pgx.Tx, l loaded, decision Decision, workerID string) error {
	if decision.ToState == "" {
		return fmt.Errorf("apply: decision %q changes no state", decision.Action)
	}

	resolved := decision.ToState != "represented"

	// The version check is the actual guard against two workers acting on one
	// dispute. The Redis lock only makes it unlikely; this makes it impossible.
	var newVersion int32
	err := tx.QueryRow(ctx, `
		UPDATE disputes
		   SET state = $3,
		       version = version + 1,
		       resolved_at = CASE WHEN $4 THEN now() ELSE resolved_at END
		 WHERE id = $1 AND version = $2
		RETURNING version`,
		l.ID, l.Version, decision.ToState, resolved,
	).Scan(&newVersion)

	if errors.Is(err, pgx.ErrNoRows) {
		return ErrStaleCandidate
	}
	if err != nil {
		return fmt.Errorf("update dispute %d: %w", l.ID, err)
	}

	detail, err := json.Marshal(map[string]any{
		"action": string(decision.Action),
		"reason": decision.Reason,
		"worker": workerID,
	})
	if err != nil {
		return fmt.Errorf("marshal decision detail: %w", err)
	}

	if _, err := tx.Exec(ctx, `
		INSERT INTO dispute_events (dispute_id, from_state, to_state, actor, detail, occurred_at)
		VALUES ($1, $2, $3, $4, $5::jsonb, now())`,
		l.ID, l.State, decision.ToState, "worker:"+workerID, string(detail),
	); err != nil {
		return fmt.Errorf("insert dispute_event: %w", err)
	}

	switch decision.Action {
	case ActionRefund:
		if err := postRefund(ctx, tx, l); err != nil {
			return err
		}
	case ActionRepresent, ActionExpire:
		// No money moves. Representing spends effort, not funds; an expiry is
		// an option lapsing, and the loss only lands if the network later rules
		// against the merchant.
	}

	payload, err := json.Marshal(map[string]any{
		"dispute_id":   l.ID,
		"merchant_id":  l.MerchantID,
		"from_state":   l.State,
		"to_state":     decision.ToState,
		"action":       string(decision.Action),
		"reason":       decision.Reason,
		"amount_minor": l.AmountMinor,
		"currency":     l.Currency,
	})
	if err != nil {
		return fmt.Errorf("marshal outbox payload: %w", err)
	}

	if _, err := tx.Exec(ctx, `
		INSERT INTO outbox (aggregate_type, aggregate_id, event_type, payload)
		VALUES ('dispute', $1, $2, $3::jsonb)`,
		l.ID, "dispute."+decision.ToState, string(payload),
	); err != nil {
		return fmt.Errorf("insert outbox: %w", err)
	}

	return nil
}

// postRefund writes the balanced journal entry that gives the money back:
// the merchant is owed less, and the money leaves the clearing account.
//
// external_ref is 'dispute:<id>:refund' and is unique. If this worker is a
// retry after a crash that committed, the insert conflicts and the whole
// transaction rolls back rather than paying twice - the conflict is the
// success condition, not an error.
func postRefund(ctx context.Context, tx pgx.Tx, l loaded) error {
	var ledgerTxID int64
	err := tx.QueryRow(ctx, `
		INSERT INTO ledger_transactions (external_ref, kind, currency, description, occurred_at, metadata)
		VALUES ($1, 'refund', $2, $3, now(), $4::jsonb)
		RETURNING id`,
		fmt.Sprintf("dispute:%d:refund", l.ID),
		l.Currency,
		"auto-refund inside the alert window",
		fmt.Sprintf(`{"reason_code":%q,"kind":%q}`, l.ReasonCode, l.Kind),
	).Scan(&ledgerTxID)
	if err != nil {
		return fmt.Errorf("post refund for dispute %d: %w", l.ID, err)
	}

	// Every parameter is cast explicitly.
	//
	// In an INSERT ... SELECT across a UNION ALL, Postgres does not infer a
	// parameter's type from the target column, and an untyped parameter
	// defaults to text: "column ledger_transaction_id is of type bigint but
	// expression is of type text". The same class of mistake as the ledger
	// lookup in the read API - a query that leaves a type open gets one chosen
	// for it, and the choice is only wrong at runtime.
	if _, err := tx.Exec(ctx, `
		INSERT INTO ledger_entries (ledger_transaction_id, account_id, direction, amount_minor, currency)
		SELECT $1::bigint, a.id, 'debit', $2::bigint, $3::char(3)
		  FROM ledger_accounts a
		 WHERE a.merchant_id = $4::bigint AND a.kind = 'merchant_balance' AND a.currency = $3::char(3)
		UNION ALL
		SELECT $1::bigint, a.id, 'credit', $2::bigint, $3::char(3)
		  FROM ledger_accounts a
		 WHERE a.merchant_id IS NULL AND a.kind = 'settlement_clearing' AND a.currency = $3::char(3)`,
		ledgerTxID, l.AmountMinor, l.Currency, l.MerchantID,
	); err != nil {
		return fmt.Errorf("post refund entries for dispute %d: %w", l.ID, err)
	}

	// The transaction's running refund total. The CHECK constraint
	// (refunded_minor <= amount_minor) is what catches a double-refund that
	// somehow got past everything above.
	if _, err := tx.Exec(ctx, `
		UPDATE transactions t
		   SET refunded_minor = t.refunded_minor + $2,
		       status = CASE WHEN t.refunded_minor + $2 >= t.amount_minor
		                     THEN 'refunded' ELSE 'partially_refunded' END
		  FROM disputes d
		 WHERE d.id = $1 AND t.id = d.transaction_id`,
		l.ID, l.AmountMinor,
	); err != nil {
		return fmt.Errorf("update refund total for dispute %d: %w", l.ID, err)
	}

	return nil
}
