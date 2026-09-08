package disputetools

import (
	"context"
	"os"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/regisoliveira/dispute-router/internal/api"
)

func liveSet(t *testing.T) *Set {
	t.Helper()
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		t.Skip("DATABASE_URL not set; skipping the chaining tests")
	}
	pool, err := pgxpool.New(context.Background(), dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)
	return New(api.NewStore(pool))
}

// Tools are chained by the caller: it reads a field out of one answer and
// passes it to the next. So a field a tool returns has to be a value the tools
// that take it will accept.
//
// This started as a bug. get_dispute returned the merchant's display name while
// get_customer_history matches on the external id, so the obvious chain -
// look up a dispute, then ask what else this customer has filed - answered
// "nothing" for every customer, including the ones with a history. Nothing
// errored: the query was valid and the answer was empty, which is the worst
// shape a wrong answer can take.
func TestTheMerchantFieldChainsBetweenTools(t *testing.T) {
	set := liveSet(t)
	ctx := context.Background()

	list, err := set.ListDisputes(ctx, ListDisputesInput{Limit: 1})
	if err != nil {
		t.Fatalf("list_disputes: %v", err)
	}
	if len(list.Disputes) == 0 {
		t.Skip("no disputes seeded")
	}

	detail, err := set.GetDispute(ctx, GetDisputeInput{ID: list.Disputes[0].ID})
	if err != nil {
		t.Fatalf("get_dispute: %v", err)
	}

	// Passing exactly what the previous tool handed back, which is the whole
	// point of the test.
	history, err := set.CustomerHistory(ctx, CustomerHistoryInput{
		Merchant:    detail.Merchant,
		CustomerRef: detail.CustomerRef,
	})
	if err != nil {
		t.Fatalf("get_customer_history: %v", err)
	}

	// A dispute always appears in its own customer's history. An empty answer
	// here means the handle did not chain.
	if history.Count == 0 {
		t.Fatalf("dispute %d is missing from its own customer's history; "+
			"merchant %q did not chain into get_customer_history",
			detail.ID, detail.Merchant)
	}
	found := false
	for _, row := range history.Disputes {
		if row.ID == detail.ID {
			found = true
		}
	}
	if !found {
		t.Errorf("dispute %d absent from its own history of %d rows", detail.ID, history.Count)
	}
}

// The same chain in the other direction: the merchant a list returns has to be
// a value the list itself will filter on.
func TestTheMerchantFieldChainsBackIntoTheFilter(t *testing.T) {
	set := liveSet(t)
	ctx := context.Background()

	list, err := set.ListDisputes(ctx, ListDisputesInput{Limit: 1})
	if err != nil {
		t.Fatalf("list_disputes: %v", err)
	}
	if len(list.Disputes) == 0 {
		t.Skip("no disputes seeded")
	}
	merchant := list.Disputes[0].Merchant

	filtered, err := set.ListDisputes(ctx, ListDisputesInput{Merchant: merchant, Limit: 1})
	if err != nil {
		t.Fatalf("list_disputes filtered: %v", err)
	}
	if len(filtered.Disputes) == 0 {
		t.Fatalf("filtering by merchant %q - the value the tool just returned - matched nothing", merchant)
	}
}

// Both are carried, and they are not the same string. If they ever become the
// same, one of them is being derived from the wrong column.
func TestTheHandleAndTheLabelAreBothPresent(t *testing.T) {
	set := liveSet(t)

	list, err := set.ListDisputes(context.Background(), ListDisputesInput{Limit: 1})
	if err != nil {
		t.Fatalf("list_disputes: %v", err)
	}
	if len(list.Disputes) == 0 {
		t.Skip("no disputes seeded")
	}
	row := list.Disputes[0]
	if row.Merchant == "" || row.MerchantName == "" {
		t.Fatalf("merchant = %q, merchant_name = %q; a reader needs the label and a tool needs the handle",
			row.Merchant, row.MerchantName)
	}
}
