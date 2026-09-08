package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// These tests write, and one of the things they write cannot be deleted, so
// they get their own database. The same reasoning as internal/agent's scratch
// harness: cleaning up would mean disabling the append-only trigger, and then
// the tests would be describing a table that does not ship.
func scratchStore(t *testing.T) (*Store, *pgxpool.Pool) {
	t.Helper()

	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		t.Skip("DATABASE_URL not set; skipping the review tests")
	}

	name := fmt.Sprintf("review_scratch_%d", time.Now().UnixNano())
	ctx := context.Background()

	admin, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	if _, err := admin.Exec(ctx, "CREATE DATABASE "+name); err != nil {
		admin.Close(ctx)
		t.Fatalf("create scratch database: %v", err)
	}
	admin.Close(ctx)

	parsed, err := url.Parse(dsn)
	if err != nil {
		t.Fatalf("parse dsn: %v", err)
	}
	parsed.Path = "/" + name

	pool, err := pgxpool.New(ctx, parsed.String())
	if err != nil {
		t.Fatalf("connect to scratch: %v", err)
	}

	conn, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	entries, err := os.ReadDir(filepath.Join("..", "..", "db", "migrations"))
	if err != nil {
		t.Fatalf("read migrations: %v", err)
	}
	for _, entry := range entries {
		if !strings.HasSuffix(entry.Name(), ".up.sql") {
			continue
		}
		body, err := os.ReadFile(filepath.Join("..", "..", "db", "migrations", entry.Name()))
		if err != nil {
			t.Fatalf("read %s: %v", entry.Name(), err)
		}
		if _, err := conn.Conn().PgConn().Exec(ctx, string(body)).ReadAll(); err != nil {
			t.Fatalf("apply %s: %v", entry.Name(), err)
		}
	}
	conn.Release()

	t.Cleanup(func() {
		pool.Close()
		admin, err := pgx.Connect(context.Background(), dsn)
		if err != nil {
			return
		}
		defer admin.Close(context.Background())
		_, _ = admin.Exec(context.Background(), "DROP DATABASE IF EXISTS "+name)
	})

	return NewStore(pool), pool
}

// awaitingReview seeds a dispute already drafted for and parked in draft_ready,
// which is the state the queue is about.
func awaitingReview(t *testing.T, pool *pgxpool.Pool, outcome string, findings string) (runID, disputeID int64) {
	t.Helper()
	ctx := context.Background()

	var merchantID, transactionID int64
	if err := pool.QueryRow(ctx, `
		INSERT INTO merchants (external_id, name, webhook_secret, currency)
		VALUES ('mrc_' || gen_random_uuid(), 'Review Test', 'whsec', 'USD') RETURNING id`).
		Scan(&merchantID); err != nil {
		t.Fatalf("merchant: %v", err)
	}
	if err := pool.QueryRow(ctx, `
		INSERT INTO transactions (merchant_id, external_id, amount_minor, currency, card_bin,
		                          card_last4, card_network, customer_ref, customer_email,
		                          descriptor, captured_at)
		VALUES ($1, 'txn_' || gen_random_uuid(), 4100, 'USD', '424242', '4242', 'visa',
		        'cus_r', 'r@example.test', 'REVIEW TEST', now() - interval '20 days')
		RETURNING id`, merchantID).Scan(&transactionID); err != nil {
		t.Fatalf("transaction: %v", err)
	}
	if err := pool.QueryRow(ctx, `
		INSERT INTO disputes (merchant_id, transaction_id, external_id, kind, card_network,
		                      reason_code, amount_minor, currency, state, deadline_at, opened_at)
		VALUES ($1, $2, 'dsp_' || gen_random_uuid(), 'chargeback', 'visa', '10.4', 4100, 'USD',
		        'draft_ready', now() + interval '48 hours', now())
		RETURNING id`, merchantID, transactionID).Scan(&disputeID); err != nil {
		t.Fatalf("dispute: %v", err)
	}
	if err := pool.QueryRow(ctx, `
		INSERT INTO agent_runs (dispute_id, attempt, model, prompt_fingerprint, tool_surface,
		                        outcome, recommendation, letter, cited_evidence, findings,
		                        input_tokens, output_tokens, cost_micros, trace, started_at)
		VALUES ($1, 1, 'test-model', 'sha256:test', ARRAY['write_representment'],
		        $2, 'represent', 'The charge was authorised.', ARRAY['receipt.pdf'], $3::jsonb,
		        3000, 400, 15000, '{}'::jsonb, now())
		RETURNING id`, disputeID, outcome, findings).Scan(&runID); err != nil {
		t.Fatalf("agent run: %v", err)
	}
	return runID, disputeID
}

func stateOf(t *testing.T, pool *pgxpool.Pool, disputeID int64) string {
	t.Helper()
	var state string
	if err := pool.QueryRow(context.Background(),
		"SELECT state FROM disputes WHERE id = $1", disputeID).Scan(&state); err != nil {
		t.Fatalf("read state: %v", err)
	}
	return state
}

// A run the verifier rejected still reaches a reviewer when it was escalated,
// and it has to appear in the queue - otherwise the escalation goes nowhere and
// the dispute expires while looking handled.
func TestTheQueueCarriesEscalatedRunsToo(t *testing.T) {
	store, pool := scratchStore(t)

	drafted, _ := awaitingReview(t, pool, "drafted", `[]`)
	escalated, _ := awaitingReview(t, pool, "rejected",
		`[{"check":"unsupported_claim","quote":"tracking 1Z999","why":"not in the record"}]`)

	rows, err := store.Reviews(context.Background(), 50)
	if err != nil {
		t.Fatalf("Reviews: %v", err)
	}

	seen := map[int64]ReviewRow{}
	for _, row := range rows {
		seen[row.RunID] = row
	}
	if _, ok := seen[drafted]; !ok {
		t.Error("a passed draft is missing from the queue")
	}
	row, ok := seen[escalated]
	if !ok {
		t.Fatal("an escalated run is missing from the queue; the escalation goes nowhere")
	}
	if row.Outcome != "rejected" {
		t.Errorf("outcome = %q; the queue must not relabel a rejected run", row.Outcome)
	}
	if row.Findings != 1 {
		t.Errorf("findings = %d; a reviewer needs to see why it was held back", row.Findings)
	}
}

// Submitting is the only path to 'represented' in the system.
func TestSubmittingRepresentsTheDispute(t *testing.T) {
	store, pool := scratchStore(t)
	ctx := context.Background()
	runID, disputeID := awaitingReview(t, pool, "drafted", `[]`)

	if err := store.Decide(ctx, runID, "submitted", "regis"); err != nil {
		t.Fatalf("Decide: %v", err)
	}
	if got := stateOf(t, pool, disputeID); got != "represented" {
		t.Errorf("state = %q, want represented", got)
	}

	detail, err := store.Review(ctx, runID)
	if err != nil {
		t.Fatalf("Review: %v", err)
	}
	if detail.Decided != "submitted" || detail.DecidedBy != "regis" || detail.DecidedAt == nil {
		t.Errorf("decision recorded as %q by %q at %v", detail.Decided, detail.DecidedBy, detail.DecidedAt)
	}
	// The letter is what was sent. It must read back exactly.
	if detail.Letter != "The charge was authorised." {
		t.Errorf("letter came back as %q", detail.Letter)
	}
}

// Discarding sends the dispute back to the queue. It is not a way to give up on
// one: writing off money is a decision this system never makes on its own, and
// there is deliberately no button for it.
func TestDiscardingReturnsTheDispute(t *testing.T) {
	store, pool := scratchStore(t)
	ctx := context.Background()
	runID, disputeID := awaitingReview(t, pool, "rejected", `[]`)

	if err := store.Decide(ctx, runID, "discarded", "regis"); err != nil {
		t.Fatalf("Decide: %v", err)
	}
	if got := stateOf(t, pool, disputeID); got != "received" {
		t.Errorf("state = %q, want received", got)
	}
}

// Two reviewers with the page open is the ordinary case, not the exotic one.
func TestASecondDecisionIsRefused(t *testing.T) {
	store, pool := scratchStore(t)
	ctx := context.Background()
	runID, disputeID := awaitingReview(t, pool, "drafted", `[]`)

	if err := store.Decide(ctx, runID, "submitted", "first"); err != nil {
		t.Fatalf("first Decide: %v", err)
	}
	err := store.Decide(ctx, runID, "discarded", "second")
	if !errors.Is(err, ErrNotReviewable) {
		t.Fatalf("second Decide err = %v, want ErrNotReviewable", err)
	}
	if got := stateOf(t, pool, disputeID); got != "represented" {
		t.Errorf("state = %q; the second decision moved a dispute it was refused for", got)
	}

	var reviewer string
	if err := pool.QueryRow(ctx,
		"SELECT reviewed_by FROM agent_runs WHERE id = $1", runID).Scan(&reviewer); err != nil {
		t.Fatalf("read reviewer: %v", err)
	}
	if reviewer != "first" {
		t.Errorf("reviewed_by = %q; the losing decision overwrote the winning one", reviewer)
	}
}

// A decision with no name behind it is not an audit trail.
func TestADecisionNeedsADecider(t *testing.T) {
	store, pool := scratchStore(t)
	runID, _ := awaitingReview(t, pool, "drafted", `[]`)

	for _, reviewer := range []string{"", "   "} {
		if err := store.Decide(context.Background(), runID, "submitted", reviewer); !errors.Is(err, ErrInvalidInput) {
			t.Errorf("Decide with reviewer %q: err = %v, want ErrInvalidInput", reviewer, err)
		}
	}
	if err := store.Decide(context.Background(), runID, "concede", "regis"); !errors.Is(err, ErrInvalidInput) {
		t.Errorf("an unknown decision was accepted: %v", err)
	}
}

// The detail reads the dispute fresh rather than from a snapshot on the run.
// The record can move between drafting and reviewing, and a reviewer deciding
// against a stale copy is deciding against something no longer true.
func TestTheDetailReadsTheDisputeFresh(t *testing.T) {
	store, pool := scratchStore(t)
	ctx := context.Background()
	runID, disputeID := awaitingReview(t, pool, "drafted", `[]`)

	if _, err := pool.Exec(ctx,
		"UPDATE disputes SET reason_code = '13.1' WHERE id = $1", disputeID); err != nil {
		t.Fatalf("change the record: %v", err)
	}

	detail, err := store.Review(ctx, runID)
	if err != nil {
		t.Fatalf("Review: %v", err)
	}
	if detail.Dispute.ReasonCode != "13.1" {
		t.Errorf("reason code = %q; the reviewer was shown a stale record", detail.Dispute.ReasonCode)
	}
}

// The findings come out of JSONB written by internal/agent. The two shapes are
// declared separately, so something has to hold them together.
func TestFindingsSurviveTheRoundTrip(t *testing.T) {
	store, pool := scratchStore(t)
	runID, _ := awaitingReview(t, pool, "rejected",
		`[{"check":"wrong_figure","quote":"$51.00","why":"the record says $41.00"}]`)

	detail, err := store.Review(context.Background(), runID)
	if err != nil {
		t.Fatalf("Review: %v", err)
	}
	if len(detail.Findings) != 1 {
		t.Fatalf("%d findings decoded", len(detail.Findings))
	}
	got := detail.Findings[0]
	if got.Check != "wrong_figure" || got.Quote != "$51.00" || !strings.Contains(got.Why, "41.00") {
		t.Errorf("finding decoded as %+v", got)
	}

	// And it re-encodes to the field names the dashboard reads.
	encoded, err := json.Marshal(got)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	for _, field := range []string{`"check"`, `"quote"`, `"why"`} {
		if !strings.Contains(string(encoded), field) {
			t.Errorf("encoded finding is missing %s: %s", field, encoded)
		}
	}
}
