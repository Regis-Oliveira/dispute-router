package worker

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/regisoliveira/dispute-router/internal/ledger"
)

// Every test here runs the real write path against a real database inside a
// transaction it rolls back. Nothing is left behind, and nothing is mocked -
// the bug this file exists for was a parameter Postgres typed as text, which no
// mock would ever have noticed.
func testPool(t *testing.T) *pgxpool.Pool {
	t.Helper()

	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		t.Skip("DATABASE_URL not set; skipping database tests")
	}

	pool, err := pgxpool.New(context.Background(), dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	if err := pool.Ping(context.Background()); err != nil {
		t.Skipf("postgres unreachable: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// openCandidate finds a real open dispute of the given kind to act on.
func openCandidate(t *testing.T, ctx context.Context, tx pgx.Tx, kind string) loaded {
	t.Helper()

	var l loaded
	err := tx.QueryRow(ctx, `
		SELECT d.id, d.merchant_id, d.kind, d.state, d.reason_code,
		       d.amount_minor, d.currency, d.deadline_at,
		       d.resolved_at IS NOT NULL, d.version,
		       m.auto_refund_ceiling_minor
		  FROM disputes d
		  JOIN merchants m ON m.id = d.merchant_id
		  JOIN transactions t ON t.id = d.transaction_id
		 WHERE d.state = 'received' AND d.kind = $1
		   AND m.auto_refund_ceiling_minor IS NOT NULL
		   -- A second alert on a transaction an earlier dispute already
		   -- refunded in full has nothing left to refund, and the constraint
		   -- says so. That is a question for the worker, not for this test.
		   AND t.refunded_minor + d.amount_minor <= t.amount_minor
		 LIMIT 1`, kind,
	).Scan(&l.ID, &l.MerchantID, &l.Kind, &l.State, &l.ReasonCode,
		&l.AmountMinor, &l.Currency, &l.DeadlineAt, &l.Resolved, &l.Version,
		&l.AutoRefundCeilingMinor)

	if err != nil {
		t.Skipf("no open %s dispute to test against (run make seed): %v", kind, err)
	}
	return l
}

// The regression test. A refund posts a balanced journal entry, moves the
// dispute to refunded, and increases the transaction's refund total - and the
// balance is proved by forcing the deferred constraint trigger to fire before
// the rollback, which is exactly what a COMMIT would have done.
func TestApplyRefundPostsABalancedEntry(t *testing.T) {
	ctx := context.Background()
	pool := testPool(t)

	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	candidate := openCandidate(t, ctx, tx, "alert")
	decision := Decision{Action: ActionRefund, ToState: "refunded", Reason: "test"}

	if err := applyTx(ctx, tx, candidate, decision, "test-worker"); err != nil {
		t.Fatalf("applyTx: %v", err)
	}

	// The balance trigger is DEFERRABLE INITIALLY DEFERRED, so it would
	// normally fire at COMMIT - which this test never reaches. Forcing it to
	// IMMEDIATE runs the same check now.
	if _, err := tx.Exec(ctx, "SET CONSTRAINTS ALL IMMEDIATE"); err != nil {
		t.Fatalf("the refund entry did not satisfy the ledger constraints: %v", err)
	}

	var state string
	var resolvedAt *time.Time
	if err := tx.QueryRow(ctx,
		`SELECT state, resolved_at FROM disputes WHERE id = $1`, candidate.ID,
	).Scan(&state, &resolvedAt); err != nil {
		t.Fatalf("re-read dispute: %v", err)
	}
	if state != "refunded" {
		t.Errorf("state = %q, want refunded", state)
	}
	if resolvedAt == nil {
		t.Error("resolved_at is null; the schema's CHECK requires it for a closed dispute")
	}

	// The ref is built in Go and bound as text.
	//
	// `$1::text` does not cast an integer to text - it declares the parameter
	// as text, and pgx is then handed an int64 it cannot encode. This is the
	// third time the same mistake has appeared in this project, in three
	// different queries; the rule that actually holds is to never let a
	// parameter's type be decided by the SQL around it.
	ref := fmt.Sprintf("dispute:%d:refund", candidate.ID)

	var debits, credits int64
	if err := tx.QueryRow(ctx, `
		SELECT COALESCE(SUM(amount_minor) FILTER (WHERE direction = 'debit'), 0),
		       COALESCE(SUM(amount_minor) FILTER (WHERE direction = 'credit'), 0)
		  FROM ledger_entries le
		  JOIN ledger_transactions lt ON lt.id = le.ledger_transaction_id
		 WHERE lt.external_ref = $1`, ref,
	).Scan(&debits, &credits); err != nil {
		t.Fatalf("read postings: %v", err)
	}

	if debits != candidate.AmountMinor {
		t.Errorf("debits = %d, want the disputed amount %d", debits, candidate.AmountMinor)
	}
	if debits != credits {
		t.Errorf("debits %d != credits %d", debits, credits)
	}
}

// The unique external_ref is the last line of defence: a worker retrying after
// a commit it did not see must not pay twice.
func TestARefundCannotBePostedTwice(t *testing.T) {
	ctx := context.Background()
	pool := testPool(t)

	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	candidate := openCandidate(t, ctx, tx, "alert")
	decision := Decision{Action: ActionRefund, ToState: "refunded", Reason: "test"}

	if err := applyTx(ctx, tx, candidate, decision, "test-worker"); err != nil {
		t.Fatalf("first applyTx: %v", err)
	}

	// Same dispute, same external_ref. Postgres has to refuse it.
	err = ledger.Refund(ctx, tx, candidate.ID, candidate.MerchantID, candidate.AmountMinor,
		candidate.Currency, time.Now(), candidate.ReasonCode)
	if err == nil {
		t.Fatal("a second refund for the same dispute was accepted")
	}
}

// Two workers reading the same version, both writing: the second must lose.
func TestApplyRefusesAStaleVersion(t *testing.T) {
	ctx := context.Background()
	pool := testPool(t)

	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	candidate := openCandidate(t, ctx, tx, "chargeback")
	decision := Decision{Action: ActionExpire, ToState: "expired", Reason: "test"}

	if err := applyTx(ctx, tx, candidate, decision, "worker-a"); err != nil {
		t.Fatalf("first applyTx: %v", err)
	}

	// worker-b is still holding the version it read before worker-a wrote.
	err = applyTx(ctx, tx, candidate, Decision{
		Action: ActionExpire, ToState: "expired", Reason: "test",
	}, "worker-b")

	if err != ErrStaleCandidate {
		t.Fatalf("second applyTx = %v, want ErrStaleCandidate", err)
	}
}
