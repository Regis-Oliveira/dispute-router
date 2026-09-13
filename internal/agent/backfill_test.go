//go:build integration

package agent

import (
	"context"
	"errors"
	"log/slog"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

// The one test here embeds a corpus it seeded itself, which it can only do in
// the scratch database scratchDB creates and drops - so the file carries
// scratchdb_test.go's integration tag.
//
// cancelOnFirstRecord is a slog.Handler that cancels a context the first time
// a record reaches it. The backfill logs once per batch, after that batch is
// written and before it checks the context, so this is the one seam where "the
// first batch is on disk and the run has been told to stop" can be arranged
// without racing a goroutine against the database.
type cancelOnFirstRecord struct {
	cancel context.CancelFunc
}

func (h *cancelOnFirstRecord) Enabled(context.Context, slog.Level) bool  { return true }
func (h *cancelOnFirstRecord) WithAttrs([]slog.Attr) slog.Handler        { return h }
func (h *cancelOnFirstRecord) WithGroup(string) slog.Handler             { return h }
func (h *cancelOnFirstRecord) Handle(context.Context, slog.Record) error { h.cancel(); return nil }

// settledFixture puts one merchant, one transaction and n won disputes with a
// claim in the scratch database: the rows a backfill has something to do with.
func settledFixture(t *testing.T, pool *pgxpool.Pool, n int) {
	t.Helper()
	ctx := t.Context()

	var merchantID int64
	if err := pool.QueryRow(ctx, `
		INSERT INTO merchants (external_id, name, webhook_secret, currency)
		VALUES ('mrc_test_' || gen_random_uuid(), 'Test Merchant', 'whsec_test', 'USD')
		RETURNING id`).Scan(&merchantID); err != nil {
		t.Fatalf("insert merchant: %v", err)
	}

	var transactionID int64
	if err := pool.QueryRow(ctx, `
		INSERT INTO transactions (merchant_id, external_id, amount_minor, currency,
		                          card_bin, card_last4, card_network, customer_ref,
		                          customer_email, descriptor, captured_at)
		VALUES ($1, 'txn_' || gen_random_uuid(), 4100, 'USD', '424242', '4242', 'visa',
		        'cus_test', 'someone@example.test', 'TEST MERCHANT', now() - interval '30 days')
		RETURNING id`, merchantID).Scan(&transactionID); err != nil {
		t.Fatalf("insert transaction: %v", err)
	}

	for i := range n {
		if _, err := pool.Exec(ctx, `
			INSERT INTO disputes (merchant_id, transaction_id, external_id, kind, card_network,
			                      reason_code, amount_minor, currency, deadline_at, opened_at,
			                      state, resolved_at, cardholder_claim)
			VALUES ($1, $2, 'dsp_' || gen_random_uuid(), 'chargeback', 'visa', '10.4', 4100, 'USD',
			        now() - interval '10 days', now() - interval '20 days',
			        'won', now() - interval '5 days', $3)`,
			merchantID, transactionID, "the parcel never arrived, claim number "+string(rune('a'+i))); err != nil {
			t.Fatalf("insert settled dispute %d: %v", i, err)
		}
	}
}

// A run cut short reports an error, not a success. cmd/embed used to print
// "done" for a backfill that had been interrupted after its first batch, with
// the numbers as the only hint that anything was left.
func TestACancelledBackfillSaysSo(t *testing.T) {
	pool := scratchDB(t)
	settledFixture(t, pool, 5)

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	embedder := &wordVector{dims: 1024}
	backfill := NewBackfill(pool, embedder, slog.New(&cancelOnFirstRecord{cancel: cancel}))

	stats, err := backfill.Run(ctx, 2, 0)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Run returned %v after cancellation, want context.Canceled", err)
	}
	if stats.Embedded != 2 || stats.Batches != 1 {
		t.Errorf("embedded %d in %d batch(es), want the first batch of 2 and nothing more", stats.Embedded, stats.Batches)
	}
	// The outstanding count is recomputed even on a cancelled run, so the
	// report is still true.
	if stats.Remaining != 3 {
		t.Errorf("remaining = %d, want 3", stats.Remaining)
	}
}
