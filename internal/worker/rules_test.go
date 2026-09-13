package worker

import (
	"strings"
	"testing"
	"time"

	"github.com/regisoliveira/dispute-router/internal/dispute"
)

var now = time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)

func ceiling(v int64) *int64 { return &v }

func alert(mutate func(*Candidate)) Candidate {
	c := Candidate{
		ID:                       1,
		Kind:                     "alert",
		State:                    "received",
		ReasonCode:               "10.4",
		AmountMinor:              2500,
		Currency:                 "USD",
		DeadlineAt:               now.Add(12 * time.Hour),
		AutoRefundCeilingMinor:   ceiling(5000),
		RefundableRemainingMinor: 1_000_000,
	}
	if mutate != nil {
		mutate(&c)
	}
	return c
}

func chargeback(mutate func(*Candidate)) Candidate {
	c := alert(nil)
	c.Kind = "chargeback"
	c.DeadlineAt = now.Add(10 * 24 * time.Hour)
	if mutate != nil {
		mutate(&c)
	}
	return c
}

func TestExpiryBeatsEveryOtherRule(t *testing.T) {
	// A dispute that would otherwise be refunded, one second past its window.
	c := alert(func(c *Candidate) { c.DeadlineAt = now.Add(-time.Second) })

	got := Decide(c, now)
	if got.Action != ActionExpire {
		t.Fatalf("Action = %q, want %q", got.Action, ActionExpire)
	}
	if got.ToState != "expired" {
		t.Errorf("ToState = %q, want expired", got.ToState)
	}
}

// The deadline is an instant, not a grace period.
func TestDeadlineIsExclusive(t *testing.T) {
	exactly := alert(func(c *Candidate) { c.DeadlineAt = now })
	if got := Decide(exactly, now); got.Action != ActionExpire {
		t.Errorf("at the deadline: Action = %q, want expire", got.Action)
	}

	justBefore := alert(func(c *Candidate) { c.DeadlineAt = now.Add(time.Millisecond) })
	if got := Decide(justBefore, now); got.Action != ActionRefund {
		t.Errorf("just before the deadline: Action = %q, want refund", got.Action)
	}
}

func TestAlertsRefundWithinTheCeiling(t *testing.T) {
	tests := map[string]struct {
		amount  int64
		ceiling *int64
		want    Action
	}{
		"well under":            {2500, ceiling(5000), ActionRefund},
		"exactly at":            {5000, ceiling(5000), ActionRefund},
		"one minor unit over":   {5001, ceiling(5000), ActionEscalate},
		"far over":              {250000, ceiling(5000), ActionEscalate},
		"no ceiling configured": {100, nil, ActionEscalate},
		"zero ceiling":          {1, ceiling(0), ActionEscalate},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			c := alert(func(c *Candidate) {
				c.AmountMinor = tc.amount
				c.AutoRefundCeilingMinor = tc.ceiling
			})
			if got := Decide(c, now); got.Action != tc.want {
				t.Errorf("Action = %q, want %q (reason: %s)", got.Action, tc.want, got.Reason)
			}
		})
	}
}

// The worker never represents. 'represented' means evidence was submitted,
// and the worker has no letter to submit: the assistant drafts one and a
// person sends it. Every chargeback with time left is left for them, whatever
// its reason code - the code only changes the reason written down.
func TestTheWorkerNeverRepresentsAChargeback(t *testing.T) {
	for _, reason := range []string{"13.1", "12.5", "4855", "C08", "RG", "10.4", "4837", "F29", "ZZ99"} {
		t.Run(reason, func(t *testing.T) {
			c := chargeback(func(c *Candidate) { c.ReasonCode = reason })
			got := Decide(c, now)
			if got.Action != ActionEscalate {
				t.Errorf("Action = %q, want escalate (reason: %s)", got.Action, got.Reason)
			}
			if got.ToState != "" {
				t.Errorf("ToState = %q; the worker moved a chargeback on its own", got.ToState)
			}
		})
	}
}

// A draft waiting on a reviewer is left alone while there is time, and expired
// like anything else once there is not. It used to be invisible to the sweeper
// entirely, so it could sit past its deadline forever with the funds still held.
func TestADraftAwaitingReviewExpiresButIsOtherwiseLeftAlone(t *testing.T) {
	waiting := chargeback(func(c *Candidate) { c.State = "draft_ready" })
	if got := Decide(waiting, now); got.Action != ActionSkip {
		t.Errorf("a draft with time left: Action = %q, want skip (%s)", got.Action, got.Reason)
	}

	overdue := chargeback(func(c *Candidate) {
		c.State = "draft_ready"
		c.DeadlineAt = now.Add(-time.Minute)
	})
	got := Decide(overdue, now)
	if got.Action != ActionExpire || got.ToState != "expired" {
		t.Errorf("a draft past its deadline: Action = %q, ToState = %q, want expire/expired", got.Action, got.ToState)
	}
}

// The rule this project would be wrong without: automation refunds alerts,
// because that is strictly cheaper than the alternative. It never concedes a
// chargeback, because that is a judgement about evidence and about a merchant
// relationship.
func TestNoRuleEverConcedesAChargeback(t *testing.T) {
	for _, reason := range []string{"10.4", "13.1", "4837", "4855", "F29", "C08", "UA02", "RG", "ZZ99"} {
		for _, amount := range []int64{1, 100, 5000, 1_000_000} {
			c := chargeback(func(c *Candidate) {
				c.ReasonCode = reason
				c.AmountMinor = amount
			})
			got := Decide(c, now)
			if got.ToState == "lost" {
				t.Fatalf("reason %s at %d: decided %q, which writes off money automatically", reason, amount, got.ToState)
			}
		}
	}
}

func TestAlreadyResolvedIsSkipped(t *testing.T) {
	for _, state := range []dispute.State{
		dispute.StateRefunded, dispute.StateWon, dispute.StateLost,
		dispute.StateExpired, dispute.StateRepresented,
	} {
		c := alert(func(c *Candidate) { c.State = state })
		if got := Decide(c, now); got.Action != ActionSkip {
			t.Errorf("state %s: Action = %q, want skip", state, got.Action)
		}
	}
}

// A dispute can be resolved between being scheduled and being claimed. The
// worker must notice rather than act on what it read a minute ago.
func TestResolvedFlagIsRespectedEvenInAnOpenState(t *testing.T) {
	c := alert(func(c *Candidate) { c.Resolved = true })
	if got := Decide(c, now); got.Action != ActionSkip {
		t.Errorf("Action = %q, want skip", got.Action)
	}
}

func TestEscalationNeverChangesState(t *testing.T) {
	c := alert(func(c *Candidate) { c.AmountMinor = 999_999 })
	got := Decide(c, now)
	if got.Action != ActionEscalate {
		t.Fatalf("Action = %q, want escalate", got.Action)
	}
	if got.ToState != "" {
		t.Errorf("ToState = %q, want empty: escalation is the worker declining to decide", got.ToState)
	}
}

func TestEveryDecisionCarriesAReason(t *testing.T) {
	candidates := []Candidate{
		alert(nil),
		alert(func(c *Candidate) { c.AmountMinor = 999_999 }),
		alert(func(c *Candidate) { c.DeadlineAt = now.Add(-time.Hour) }),
		chargeback(func(c *Candidate) { c.ReasonCode = "13.1" }),
		chargeback(func(c *Candidate) { c.ReasonCode = "10.4" }),
		alert(func(c *Candidate) { c.State = "refunded" }),
		alert(func(c *Candidate) { c.Kind = "retrieval" }),
	}
	for _, c := range candidates {
		if got := Decide(c, now); got.Reason == "" {
			t.Errorf("%+v produced a decision with no reason", c)
		}
	}
}

// Several disputes can point at one transaction, and their refunds share the
// budget of what is left on the original charge. The database's CHECK is the
// backstop; deciding it here is what stops the worker retrying a permanent
// condition forever.
func TestRefundsCannotExceedWhatIsLeftOnTheCharge(t *testing.T) {
	tests := map[string]struct {
		amount    int64
		remaining int64
		want      Action
	}{
		"room to spare":          {2500, 10000, ActionRefund},
		"exactly the last of it": {2500, 2500, ActionRefund},
		"one minor unit short":   {2500, 2499, ActionEscalate},
		"nothing left":           {2500, 0, ActionClose},
		"already over-refunded":  {2500, -100, ActionClose},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			c := alert(func(c *Candidate) {
				c.AmountMinor = tc.amount
				c.RefundableRemainingMinor = tc.remaining
			})
			got := Decide(c, now)
			if got.Action != tc.want {
				t.Errorf("Action = %q, want %q (reason: %s)", got.Action, tc.want, got.Reason)
			}
		})
	}
}

// The ceiling is the merchant's policy and the remaining balance is a fact
// about the money. Both have to hold, and the ceiling is checked first so the
// reason given is the one an operator can act on.
func TestCeilingIsReportedBeforeTheBalance(t *testing.T) {
	c := alert(func(c *Candidate) {
		c.AmountMinor = 999_999
		c.RefundableRemainingMinor = 100
	})
	got := Decide(c, now)
	if got.Action != ActionEscalate {
		t.Fatalf("Action = %q, want escalate", got.Action)
	}
	if got.Reason != "alert above the merchant's auto-refund ceiling" {
		t.Errorf("Reason = %q, want the ceiling to be reported first", got.Reason)
	}
}

// An alert on a charge that was already refunded in full is answered. It
// closes as refunded, moves no money, and does so whatever the merchant's
// ceiling says, because nothing is being refunded for a ceiling to bound.
func TestAnAlertOnAChargeAlreadyRefundedIsClosedWithoutMoney(t *testing.T) {
	for name, mutate := range map[string]func(*Candidate){
		"within the ceiling": func(c *Candidate) {},
		"above the ceiling":  func(c *Candidate) { c.AmountMinor = 999_999 },
		"no ceiling at all":  func(c *Candidate) { c.AutoRefundCeilingMinor = nil },
	} {
		t.Run(name, func(t *testing.T) {
			c := alert(func(c *Candidate) {
				mutate(c)
				c.RefundableRemainingMinor = 0
			})
			got := Decide(c, now)
			if got.Action != ActionClose {
				t.Fatalf("Action = %q, want close (reason: %s)", got.Action, got.Reason)
			}
			if got.ToState != "refunded" {
				t.Errorf("ToState = %q, want refunded", got.ToState)
			}
			if got.Reason == "" {
				t.Error("no reason")
			}
		})
	}
}

// The same charge under a chargeback is still a person's decision - the
// letter is "credit already issued", and the worker writes no letters - but
// the reason names what they will find.
func TestAChargebackOnAChargeAlreadyRefundedStillEscalates(t *testing.T) {
	c := chargeback(func(c *Candidate) { c.RefundableRemainingMinor = 0 })
	got := Decide(c, now)
	if got.Action != ActionEscalate || got.ToState != "" {
		t.Fatalf("decision = %+v, want escalate with no state change", got)
	}
	if !strings.Contains(got.Reason, "already refunded in full") {
		t.Errorf("Reason = %q does not say the charge was refunded", got.Reason)
	}
}

// nextVisit is the invariant that keeps the pool from livelocking: anything
// scored at or before now+lookahead is claimable right now, so a reschedule
// must land strictly outside that window.
func TestNextVisitAlwaysClearsTheClaimWindow(t *testing.T) {
	for _, lookahead := range []time.Duration{0, 30 * time.Second, time.Hour, 720 * time.Hour} {
		p := NewPool(Options{Lookahead: lookahead})

		for _, preferred := range []time.Time{
			{},                             // no preference (failure path)
			time.Now().Add(-time.Hour),     // already overdue
			time.Now(),                     // right now
			time.Now().Add(time.Second),    // inside any window
			time.Now().Add(lookahead / 2),  // halfway into the window
			time.Now().Add(lookahead),      // exactly at the edge
			time.Now().Add(lookahead * 10), // well beyond it
		} {
			at := p.nextVisit(preferred)
			window := time.Now().Add(lookahead)
			if !at.After(window) {
				t.Errorf("lookahead %s, preferred %v: nextVisit returned %v, which is inside the claim window ending %v",
					lookahead, preferred, at, window)
			}
		}
	}
}

// A preference beyond the window is honoured rather than pulled forward.
func TestNextVisitKeepsALaterPreference(t *testing.T) {
	p := NewPool(Options{Lookahead: time.Minute})
	preferred := time.Now().Add(48 * time.Hour)

	if got := p.nextVisit(preferred); !got.Equal(preferred) {
		t.Errorf("nextVisit = %v, want the preferred %v", got, preferred)
	}
}
