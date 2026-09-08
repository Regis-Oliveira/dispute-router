package agent

import (
	"context"
	"errors"
	"testing"
	"time"
)

func aRun(outcome string, escalated bool) Run {
	return Run{
		Model: "test-model", PromptFingerprint: "sha256:test",
		ToolSurface: []string{RepresentmentTool, VerdictTool},
		Outcome:     outcome, Recommendation: RecommendRepresent,
		Letter:     "a letter",
		Escalated:  escalated,
		Usage:      Usage{InputTokens: 3000, OutputTokens: 400},
		CostMicros: 15_000,
		Trace:      map[string]any{"note": "test"},
		StartedAt:  time.Now(),
	}
}

// Everything after the hold costs money, so the race is settled before any is
// spent. The loser has paid for nothing.
func TestOnlyOneHolderGetsADispute(t *testing.T) {
	pool := scratchDB(t)
	runs := NewRuns(pool)
	id := fixture(t, pool, "chargeback", 72*time.Hour)

	first, err := runs.Hold(context.Background(), id)
	if err != nil {
		t.Fatalf("first hold: %v", err)
	}
	if first.Attempt != 1 {
		t.Errorf("attempt = %d, want 1", first.Attempt)
	}
	if got := stateOf(t, pool, id); got != "resolving" {
		t.Errorf("state = %q after a hold, want resolving", got)
	}

	if _, err := runs.Hold(context.Background(), id); !errors.Is(err, ErrClaimLost) {
		t.Fatalf("second hold err = %v, want ErrClaimLost", err)
	}
}

// The run and the move are one fact. A run recorded against a dispute that
// never moved is a draft nobody will look at; a dispute moved with no run
// behind it is a state change with no explanation.
func TestRecordIsAtomic(t *testing.T) {
	pool := scratchDB(t)
	runs := NewRuns(pool)
	ctx := context.Background()
	id := fixture(t, pool, "chargeback", 72*time.Hour)

	claim, err := runs.Hold(ctx, id)
	if err != nil {
		t.Fatalf("hold: %v", err)
	}

	// Somebody else moves the dispute while the model was thinking, so the
	// version the claim carries is now stale.
	if _, err := pool.Exec(ctx,
		"UPDATE disputes SET version = version + 1 WHERE id = $1", id); err != nil {
		t.Fatalf("simulate a concurrent change: %v", err)
	}

	if err := runs.Record(ctx, claim, aRun(OutcomeDrafted, false)); !errors.Is(err, ErrClaimLost) {
		t.Fatalf("Record err = %v, want ErrClaimLost", err)
	}
	if n := runNum(t, pool, id); n != 0 {
		t.Errorf("%d agent_runs left behind by a rolled-back Record; the insert did not roll back with the update", n)
	}
}

func TestRecordWritesTheRunTheMoveAndTheEvent(t *testing.T) {
	pool := scratchDB(t)
	runs := NewRuns(pool)
	ctx := context.Background()
	id := fixture(t, pool, "chargeback", 72*time.Hour)

	claim, err := runs.Hold(ctx, id)
	if err != nil {
		t.Fatalf("hold: %v", err)
	}
	if err := runs.Record(ctx, claim, aRun(OutcomeDrafted, false)); err != nil {
		t.Fatalf("Record: %v", err)
	}

	if got := stateOf(t, pool, id); got != "draft_ready" {
		t.Errorf("state = %q, want draft_ready", got)
	}
	if n := runNum(t, pool, id); n != 1 {
		t.Errorf("%d agent_runs, want 1", n)
	}

	var actor, toState string
	if err := pool.QueryRow(ctx, `
		SELECT actor, to_state FROM dispute_events
		 WHERE dispute_id = $1 ORDER BY id DESC LIMIT 1`, id).Scan(&actor, &toState); err != nil {
		t.Fatalf("read event: %v", err)
	}
	if actor != "agent" || toState != "draft_ready" {
		t.Errorf("event actor=%q to_state=%q", actor, toState)
	}
}

// Escalation changes where a dispute goes, never what the run says. Recording
// a rejected draft as 'drafted' would put a letter the verifier refused into a
// queue labelled as checked.
func TestEscalationRoutesWithoutRelabelling(t *testing.T) {
	pool := scratchDB(t)
	runs := NewRuns(pool)
	ctx := context.Background()
	id := fixture(t, pool, "chargeback", 72*time.Hour)

	claim, err := runs.Hold(ctx, id)
	if err != nil {
		t.Fatalf("hold: %v", err)
	}
	run := aRun(OutcomeRejected, true)
	run.Findings = []Finding{{Check: CheckUnsupportedClaim, Quote: "x", Why: "invented"}}
	if err := runs.Record(ctx, claim, run); err != nil {
		t.Fatalf("Record: %v", err)
	}

	if got := stateOf(t, pool, id); got != "draft_ready" {
		t.Errorf("state = %q, want draft_ready; an escalated run goes to a person", got)
	}

	var outcome string
	var findings int
	if err := pool.QueryRow(ctx, `
		SELECT outcome, jsonb_array_length(findings) FROM agent_runs WHERE dispute_id = $1`,
		id).Scan(&outcome, &findings); err != nil {
		t.Fatalf("read run: %v", err)
	}
	if outcome != OutcomeRejected {
		t.Errorf("outcome = %q, want %q; escalation must not rename the verdict", outcome, OutcomeRejected)
	}
	if findings != 1 {
		t.Errorf("%d findings stored; the reviewer needs to see why it was held back", findings)
	}
}

// A rejection below the ceiling goes back to the queue rather than to a person.
func TestARejectedRunReturnsTheDispute(t *testing.T) {
	pool := scratchDB(t)
	runs := NewRuns(pool)
	ctx := context.Background()
	id := fixture(t, pool, "chargeback", 72*time.Hour)

	claim, err := runs.Hold(ctx, id)
	if err != nil {
		t.Fatalf("hold: %v", err)
	}
	if err := runs.Record(ctx, claim, aRun(OutcomeRejected, false)); err != nil {
		t.Fatalf("Record: %v", err)
	}
	if got := stateOf(t, pool, id); got != "received" {
		t.Errorf("state = %q, want received", got)
	}
}

func TestReleasePutsADisputeBack(t *testing.T) {
	pool := scratchDB(t)
	runs := NewRuns(pool)
	ctx := context.Background()
	id := fixture(t, pool, "chargeback", 72*time.Hour)

	claim, err := runs.Hold(ctx, id)
	if err != nil {
		t.Fatalf("hold: %v", err)
	}
	if err := runs.Release(ctx, claim); err != nil {
		t.Fatalf("Release: %v", err)
	}
	if got := stateOf(t, pool, id); got != "received" {
		t.Errorf("state = %q, want received", got)
	}
	if n := runNum(t, pool, id); n != 0 {
		t.Errorf("a released dispute recorded %d runs; a fault is not a verdict", n)
	}
}

// An alert is cheaper to refund than to fight, which is already the rule in
// the worker. Drafting for one would spend tokens arguing against money that
// has not been taken yet.
func TestAlertsAreNotCandidates(t *testing.T) {
	pool := scratchDB(t)
	runs := NewRuns(pool)
	ctx := context.Background()

	alert := fixture(t, pool, "alert", 72*time.Hour)
	chargeback := fixture(t, pool, "chargeback", 72*time.Hour)

	ids, err := runs.Candidates(ctx, 2, 10)
	if err != nil {
		t.Fatalf("Candidates: %v", err)
	}
	if containsID(ids, alert) {
		t.Error("an alert was offered as a candidate")
	}
	if !containsID(ids, chargeback) {
		t.Error("an open chargeback was not offered as a candidate")
	}
}

// Past the deadline there is nothing to win, and the attempt ceiling is what
// stops a dispute the verifier keeps rejecting from being redrafted forever.
func TestCandidatesRespectTheDeadlineAndTheCeiling(t *testing.T) {
	pool := scratchDB(t)
	runs := NewRuns(pool)
	ctx := context.Background()

	expired := fixture(t, pool, "chargeback", -time.Hour)
	exhausted := fixture(t, pool, "chargeback", 72*time.Hour)

	// Two attempts already made and returned to the queue.
	for i := 0; i < 2; i++ {
		claim, err := runs.Hold(ctx, exhausted)
		if err != nil {
			t.Fatalf("hold %d: %v", i, err)
		}
		if err := runs.Record(ctx, claim, aRun(OutcomeRejected, false)); err != nil {
			t.Fatalf("record %d: %v", i, err)
		}
	}

	ids, err := runs.Candidates(ctx, 2, 10)
	if err != nil {
		t.Fatalf("Candidates: %v", err)
	}
	if containsID(ids, expired) {
		t.Error("a dispute past its deadline was offered as a candidate")
	}
	if containsID(ids, exhausted) {
		t.Error("a dispute at the attempt ceiling was offered again")
	}
}

func containsID(ids []int64, want int64) bool {
	for _, id := range ids {
		if id == want {
			return true
		}
	}
	return false
}
