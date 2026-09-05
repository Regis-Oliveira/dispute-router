package api

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// These run against a real database rather than a mock. The queries here are
// the product - a mock would assert that the strings are unchanged, not that
// Postgres accepts them, and it is exactly a scan mismatch that a mock cannot
// see.
func testStore(t *testing.T) *Store {
	t.Helper()

	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		t.Skip("DATABASE_URL not set; skipping database tests")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	if err := pool.Ping(ctx); err != nil {
		t.Fatalf("ping: %v", err)
	}
	t.Cleanup(pool.Close)

	return NewStore(pool)
}

func firstDisputeID(t *testing.T, store *Store) int64 {
	t.Helper()

	list, err := store.ListDisputes(context.Background(), Filters{Limit: 1, Sort: defaultSort})
	if err != nil {
		t.Fatalf("ListDisputes: %v", err)
	}
	if len(list.Rows) == 0 {
		t.Skip("no disputes seeded; run make seed")
	}
	return list.Rows[0].ID
}

func TestListDisputes(t *testing.T) {
	store := testStore(t)

	list, err := store.ListDisputes(context.Background(), Filters{Limit: 5, Sort: defaultSort})
	if err != nil {
		t.Fatalf("ListDisputes: %v", err)
	}
	if len(list.Rows) == 0 {
		t.Skip("no disputes seeded; run make seed")
	}

	row := list.Rows[0]
	if row.Amount.AmountMinor <= 0 {
		t.Errorf("amount_minor = %d, want positive", row.Amount.AmountMinor)
	}
	if len(row.Amount.Currency) != 3 {
		t.Errorf("currency = %q, want a 3-letter code", row.Amount.Currency)
	}
	if row.MerchantName == "" {
		t.Error("merchant_name is empty; the display join is not being applied")
	}
}

// The regression this file was written for: every column the detail query
// selects has to land in a Go field of a type pgx can scan it into, and the
// only way to find out is to run it.
func TestDispute(t *testing.T) {
	store := testStore(t)
	id := firstDisputeID(t, store)

	detail, err := store.Dispute(context.Background(), id)
	if err != nil {
		t.Fatalf("Dispute(%d): %v", id, err)
	}

	if detail.ID != id {
		t.Errorf("id = %d, want %d", detail.ID, id)
	}
	if detail.CustomerEmail == "" {
		t.Error("customer_email is empty")
	}
	if detail.OriginalAmount.Currency != detail.Amount.Currency {
		t.Errorf("original amount currency = %q, want %q",
			detail.OriginalAmount.Currency, detail.Amount.Currency)
	}
	// Seeded disputes always have at least the 'received' event.
	if len(detail.Events) == 0 {
		t.Error("events is empty; the history query returned nothing")
	}
}

func TestDisputeNotFound(t *testing.T) {
	store := testStore(t)

	if _, err := store.Dispute(context.Background(), -1); err == nil {
		t.Fatal("Dispute(-1) returned no error, want ErrNotFound")
	}
}

func TestSummary(t *testing.T) {
	store := testStore(t)

	summary, err := store.Summary(context.Background(), Filters{})
	if err != nil {
		t.Fatalf("Summary: %v", err)
	}
	// Every bucket is emitted, including the empty ones, so a chart's bars
	// never appear and disappear between refreshes.
	if len(summary.Deadlines) != 5 {
		t.Errorf("deadline buckets = %d, want 5", len(summary.Deadlines))
	}
	if len(summary.Daily) != 30 {
		t.Errorf("daily points = %d, want 30", len(summary.Daily))
	}
}
