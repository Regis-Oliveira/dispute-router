package api

import (
	"context"
	"testing"
)

func decided(t *testing.T, store *Store, runID int64, decision, reviewer string) {
	t.Helper()
	if err := store.Decide(context.Background(), runID, decision, reviewer); err != nil {
		t.Fatalf("Decide: %v", err)
	}
}

// The pair the screen exists for: the verifier refused the letter and a person
// sent it anyway. Computed in the API rather than left to a client, so every
// reader agrees on what an override is.
func TestAnOverrideIsMarked(t *testing.T) {
	store, pool := scratchStore(t)
	ctx := context.Background()

	rejected, _ := awaitingReview(t, pool, "rejected",
		`[{"check":"unsupported_claim","quote":"tracking 1Z999","why":"not in the record"}]`)
	clean, _ := awaitingReview(t, pool, "drafted", `[]`)

	decided(t, store, rejected, "submitted", "Regis")
	decided(t, store, clean, "submitted", "Regis")

	list, err := store.Decisions(ctx, DecisionFilters{})
	if err != nil {
		t.Fatalf("Decisions: %v", err)
	}
	if list.Page.Total != 2 {
		t.Fatalf("total = %d, want 2", list.Page.Total)
	}

	byRun := map[int64]DecisionRow{}
	for _, row := range list.Rows {
		byRun[row.RunID] = row
	}
	if !byRun[rejected].Override {
		t.Error("a rejected draft that was submitted was not marked an override")
	}
	if byRun[clean].Override {
		t.Error("a draft the verifier passed was marked an override")
	}

	only, err := store.Decisions(ctx, DecisionFilters{OnlyOverrides: true})
	if err != nil {
		t.Fatalf("Decisions: %v", err)
	}
	if only.Page.Total != 1 || only.Rows[0].RunID != rejected {
		t.Errorf("overrides filter returned %d rows: %+v", only.Page.Total, only.Rows)
	}
}

// A discarded rejection is not an override: the person agreed with the verifier.
func TestDiscardingARejectionIsNotAnOverride(t *testing.T) {
	store, pool := scratchStore(t)
	runID, _ := awaitingReview(t, pool, "rejected", `[]`)
	decided(t, store, runID, "discarded", "Regis")

	list, err := store.Decisions(context.Background(), DecisionFilters{})
	if err != nil {
		t.Fatalf("Decisions: %v", err)
	}
	if list.Rows[0].Override {
		t.Error("agreeing with the verifier was recorded as overriding it")
	}
}

// The reviewer is an identity, not a search term. It is a typed name today and
// a user id once there is a login; matching it loosely now would build in the
// assumption that has to go.
func TestTheReviewerIsMatchedExactly(t *testing.T) {
	store, pool := scratchStore(t)
	ctx := context.Background()

	mine, _ := awaitingReview(t, pool, "drafted", `[]`)
	theirs, _ := awaitingReview(t, pool, "drafted", `[]`)
	decided(t, store, mine, "submitted", "Regis")
	decided(t, store, theirs, "submitted", "Ricardo")

	list, err := store.Decisions(ctx, DecisionFilters{Reviewer: "Regis"})
	if err != nil {
		t.Fatalf("Decisions: %v", err)
	}
	if list.Page.Total != 1 || list.Rows[0].RunID != mine {
		t.Errorf("exact match returned %d rows", list.Page.Total)
	}

	// Different case is a different identity. Today that reads as a data
	// quality problem; once these are user ids it is simply correct.
	lower, err := store.Decisions(ctx, DecisionFilters{Reviewer: "regis"})
	if err != nil {
		t.Fatalf("Decisions: %v", err)
	}
	if lower.Page.Total != 0 {
		t.Errorf("a different identity matched %d rows; the filter is not exact", lower.Page.Total)
	}

	// And a prefix is not a match either.
	partial, err := store.Decisions(ctx, DecisionFilters{Reviewer: "Reg"})
	if err != nil {
		t.Fatalf("Decisions: %v", err)
	}
	if partial.Page.Total != 0 {
		t.Errorf("a prefix matched %d rows; the filter is doing a search", partial.Page.Total)
	}
}

// Selecting one reviewer must not remove everybody else from the control that
// selected them.
func TestTheReviewerListIgnoresTheActiveFilter(t *testing.T) {
	store, pool := scratchStore(t)
	ctx := context.Background()

	a, _ := awaitingReview(t, pool, "drafted", `[]`)
	b, _ := awaitingReview(t, pool, "drafted", `[]`)
	decided(t, store, a, "submitted", "Regis")
	decided(t, store, b, "discarded", "Ricardo")

	list, err := store.Decisions(ctx, DecisionFilters{Reviewer: "Regis"})
	if err != nil {
		t.Fatalf("Decisions: %v", err)
	}
	if len(list.Reviewers) != 2 {
		t.Errorf("reviewers = %v; the filter narrowed the list of people to filter by", list.Reviewers)
	}
}

// A run nobody has decided on belongs to the queue, not the history.
func TestUndecidedRunsAreNotHistory(t *testing.T) {
	store, pool := scratchStore(t)
	awaitingReview(t, pool, "drafted", `[]`)

	list, err := store.Decisions(context.Background(), DecisionFilters{})
	if err != nil {
		t.Fatalf("Decisions: %v", err)
	}
	if list.Page.Total != 0 {
		t.Errorf("an undecided run appeared in the decision history")
	}
}
