package worker

import (
	"context"
	"testing"
	"time"
)

// Loads real open disputes and reports what the policy decides about each.
// Not an assertion about the seed data - a check that Load and Decide agree,
// which is where a field that is read but never populated shows up.
func TestDecisionsOverRealOpenDisputes(t *testing.T) {
	ctx := context.Background()
	pool := testPool(t)
	store := NewStore(pool)

	open, err := store.OpenDeadlines(ctx)
	if err != nil {
		t.Fatalf("OpenDeadlines: %v", err)
	}
	if len(open) == 0 {
		t.Skip("no open disputes; run make seed")
	}

	counts := map[Action]int{}
	reasons := map[string]int{}
	checked := 0

	for id := range open {
		if checked >= 400 {
			break
		}
		checked++

		l, err := store.Load(ctx, id)
		if err != nil {
			t.Fatalf("Load(%d): %v", id, err)
		}
		d := Decide(l.Candidate, time.Now())
		counts[d.Action]++
		reasons[d.Reason]++
	}

	t.Logf("over %d real open disputes:", checked)
	for action, n := range counts {
		t.Logf("  %-10s %d", action, n)
	}
	for reason, n := range reasons {
		t.Logf("  %4d  %s", n, reason)
	}

	if counts[ActionRefund] == 0 && counts[ActionRepresent] == 0 {
		t.Error("the policy decided nothing actionable over 400 open disputes; Load and Decide disagree somewhere")
	}
}
