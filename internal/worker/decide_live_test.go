package worker

import (
	"errors"
	"strings"
	"testing"
	"time"
)

// Runs Load and Decide over real disputes.
//
// The property under test is that the two agree: every field Decide reads is a
// field Load populates. The first version of this asserted that some disputes
// were still actionable, which is a fact about the data rather than the code -
// it passed on a fresh seed and failed the moment the worker had done its job.
// A test that only holds before the system runs is worse than no test.
func TestLoadPopulatesEverythingDecideReads(t *testing.T) {
	ctx := t.Context()
	store := NewStore(testPool(t))

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

		l, err := store.load(ctx, id)
		// internal/ingest's handler tests seed a dispute into this same database
		// and delete it again, and `go test ./...` runs that package beside this
		// one - so an id listed a moment ago can be gone by the time it is
		// loaded. The claim here is about which columns Load fills in, not about
		// the dataset holding still while it runs.
		if errors.Is(err, ErrNotFound) {
			continue
		}
		if err != nil {
			t.Fatalf("Load(%d): %v", id, err)
		}
		checked++

		// A field left at its zero value is how a query that forgot a column
		// turns into a policy that quietly escalates everything.
		switch {
		case l.Kind == "":
			t.Fatalf("dispute %d: Kind is empty", id)
		case l.State == "":
			t.Fatalf("dispute %d: State is empty", id)
		case l.ReasonCode == "":
			t.Fatalf("dispute %d: ReasonCode is empty", id)
		case l.AmountMinor <= 0:
			t.Fatalf("dispute %d: AmountMinor is %d", id, l.AmountMinor)
		case len(l.Currency) != 3:
			t.Fatalf("dispute %d: Currency is %q", id, l.Currency)
		case l.DeadlineAt.IsZero():
			t.Fatalf("dispute %d: DeadlineAt is zero", id)
		case l.MerchantID == 0:
			t.Fatalf("dispute %d: MerchantID is zero", id)
		}

		decision := Decide(l.Candidate, time.Now())
		counts[decision.Action]++
		reasons[decision.Reason]++

		// These two reasons mean the policy met something it does not
		// recognise, which is a gap in the code rather than a judgement call.
		if strings.Contains(decision.Reason, "unrecognised reason code") {
			t.Errorf("dispute %d: reason code %q is not in the category map", id, l.ReasonCode)
		}
		if strings.Contains(decision.Reason, "unknown dispute kind") {
			t.Errorf("dispute %d: kind %q is not handled", id, l.Kind)
		}
	}

	t.Logf("over %d real open disputes:", checked)
	for action, n := range counts {
		t.Logf("  %-10s %d", action, n)
	}
	for reason, n := range reasons {
		t.Logf("  %4d  %s", n, reason)
	}
}
