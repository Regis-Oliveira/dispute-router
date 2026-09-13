package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/regisoliveira/dispute-router/internal/dispute"
	"github.com/regisoliveira/dispute-router/internal/events"
	"github.com/regisoliveira/dispute-router/internal/llm"
)

// ErrClaimLost means another process moved the dispute between the read and
// the write. Not a failure: the version check did its job, the same way it does
// for the worker.
var ErrClaimLost = errors.New("agent: dispute changed under the claim")

// Runs is the agent's persistence: what it is allowed to work on, and what it
// wrote down afterwards.
type Runs struct{ pool *pgxpool.Pool }

// NewRuns binds the agent's persistence to a pool.
func NewRuns(pool *pgxpool.Pool) *Runs { return &Runs{pool: pool} }

// Claim is one dispute, held.
type Claim struct {
	DisputeID int64
	Version   int32
	Attempt   int

	// SpentMicros is what earlier attempts at this dispute already cost. The
	// ceiling is per dispute, and a ceiling that resets every attempt is a
	// ceiling times the attempt count.
	SpentMicros int64

	// PriorFindings is what the verifier said about the previous attempt's
	// draft, so the next one is not a blind re-roll. Empty on a first attempt.
	PriorFindings []Finding
}

// Candidates lists disputes worth drafting for.
//
// Chargebacks only: an alert is cheaper to refund than to fight, which is
// already the rule in internal/worker/rules.go, and drafting a representment
// for one would be spending model tokens to argue against money that has not
// been taken yet.
//
// A dispute due in the next five minutes is left alone. A draft is two model
// calls and a person's attention, and a deadline that close will pass before
// anyone reads it - the sweeper would expire the dispute under a run that had
// already paid for it. The candidates are ordered by deadline, so without this
// the ones about to expire would be drafted first.
//
// The attempt ceiling is what stops a dispute the verifier keeps rejecting from
// being redrafted forever. Past it, the last run goes in front of a person -
// escalation here means a human, because there is nothing else for it to mean.
func (r *Runs) Candidates(ctx context.Context, maxAttempts, limit int) ([]int64, error) {
	// 'received' is spelled out rather than bound: disputes_open_deadline_idx
	// is partial on state IN ('received','resolving'), and the planner can only
	// use it when the literal lets it prove the predicate holds.
	rows, err := r.pool.Query(ctx, `
		SELECT d.id
		  FROM disputes d
		 WHERE d.state = 'received'
		   AND d.kind  = $3
		   AND d.deadline_at > now() + interval '5 minutes'
		   AND (SELECT count(*) FROM agent_runs a WHERE a.dispute_id = d.id) < $1
		 ORDER BY d.deadline_at
		 LIMIT $2`, maxAttempts, limit, dispute.KindChargeback)
	if err != nil {
		return nil, fmt.Errorf("agent candidates: %w", err)
	}
	defer rows.Close()

	ids := []int64{}
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("scan candidate: %w", err)
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// Hold takes a dispute out of the queue before any money is spent.
//
// The order matters. Everything after this costs real tokens, so the race is
// settled first: the version check means exactly one process gets past this
// line, and the loser has paid for nothing. Settling it afterwards - letting
// both draft and having the unique key reject the second - would be correct in
// the database and wasteful everywhere else.
func (r *Runs) Hold(ctx context.Context, disputeID int64) (Claim, error) {
	var claim Claim
	var priorFindings string
	err := r.pool.QueryRow(ctx, `
		WITH held AS (
		  UPDATE disputes
		     SET state = $2, version = version + 1
		   WHERE id = $1 AND state = $3
		  RETURNING id, version
		)
		SELECT h.id, h.version,
		       (SELECT count(*) FROM agent_runs a WHERE a.dispute_id = h.id) + 1,
		       (SELECT coalesce(sum(a.cost_micros), 0) FROM agent_runs a WHERE a.dispute_id = h.id),
		       coalesce((SELECT a.findings::text FROM agent_runs a WHERE a.dispute_id = h.id
		                  ORDER BY a.attempt DESC LIMIT 1), '[]')
		  FROM held h`, disputeID, dispute.StateResolving, dispute.StateReceived).
		Scan(&claim.DisputeID, &claim.Version, &claim.Attempt, &claim.SpentMicros, &priorFindings)

	if errors.Is(err, pgx.ErrNoRows) {
		return Claim{}, ErrClaimLost
	}
	if err != nil {
		return Claim{}, fmt.Errorf("hold dispute %d: %w", disputeID, err)
	}
	if err := json.Unmarshal([]byte(priorFindings), &claim.PriorFindings); err != nil {
		return Claim{}, fmt.Errorf("hold dispute %d: decode prior findings: %w", disputeID, err)
	}
	return claim, nil
}

// Release puts a dispute back when the run produced nothing worth reviewing.
func (r *Runs) Release(ctx context.Context, claim Claim) error {
	tag, err := r.pool.Exec(ctx, `
		UPDATE disputes SET state = $3, version = version + 1
		 WHERE id = $1 AND version = $2`, claim.DisputeID, claim.Version, dispute.StateReceived)
	if err != nil {
		return fmt.Errorf("release dispute %d: %w", claim.DisputeID, err)
	}
	if tag.RowsAffected() == 0 {
		return ErrClaimLost
	}
	return nil
}

// Outcome is what one attempt produced. It mirrors the CHECK on
// agent_runs.outcome, in full: OutcomeFailed is never written by this package
// (a fault is not recorded as a run) but the constraint names it, and a mirror
// that omits a value stops being one.
//
// The strings are the database's. A value changed here without a migration is
// a write Postgres refuses at runtime, and a constant added here without one
// is the same.
type Outcome string

const (
	// OutcomeDrafted is a letter the verifier passed.
	OutcomeDrafted Outcome = "drafted"

	// OutcomeRejected is a letter the verifier, or the citation check before
	// it, found something in. The letter is kept; it goes to a person.
	OutcomeRejected Outcome = "rejected"

	// OutcomeInsufficient is the generator declining: the record does not
	// support a rebuttal. A real answer, not a failure.
	OutcomeInsufficient Outcome = "insufficient_evidence"

	// OutcomeBudget means the ceiling was reached before the run could finish.
	// agent_runs refuses a recommendation on such a row, because nothing
	// checked whatever letter was paid for.
	OutcomeBudget Outcome = "budget_exceeded"

	// OutcomeFailed is a fault rather than an outcome: no draft and no
	// verdict. Never written - a run that fails is released, not recorded -
	// and kept because the CHECK constraint names it.
	OutcomeFailed Outcome = "failed"
)

// Run is everything one attempt produced.
type Run struct {
	Model             string
	PromptFingerprint string
	ToolSurface       []string
	Outcome           Outcome
	Recommendation    Recommendation
	Letter            string
	CitedEvidence     []string
	Findings          []Finding
	Usage             llm.Usage
	CostMicros        int64
	Trace             *Trace
	StartedAt         time.Time

	// Escalated routes a run to a person without changing what it says. A
	// draft the verifier rejected on the last allowed attempt still goes in
	// front of a reviewer, but it is recorded as rejected, because that is
	// what happened. Renaming it to 'drafted' on the way past would put a
	// letter the verifier refused in a queue labelled as checked.
	Escalated bool
}

// Record writes the run and moves the dispute, together.
//
// One transaction, because the two halves are one fact. A run recorded against
// a dispute that never moved is a draft nobody will look at; a dispute moved
// with no run behind it is a state change with no explanation, which is the
// exact thing agent_runs exists to prevent.
func (r *Runs) Record(ctx context.Context, claim Claim, run Run) error {
	toState, awaitingReview := nextState(run.Outcome, run.Escalated)

	findings := run.Findings
	if findings == nil {
		findings = []Finding{}
	}
	encodedFindings, err := json.Marshal(findings)
	if err != nil {
		return fmt.Errorf("encode findings: %w", err)
	}
	encodedTrace, err := json.Marshal(run.Trace)
	if err != nil {
		return fmt.Errorf("encode trace: %w", err)
	}

	cited := run.CitedEvidence
	if cited == nil {
		cited = []string{}
	}
	surface := run.ToolSurface
	if surface == nil {
		surface = []string{}
	}

	return pgx.BeginFunc(ctx, r.pool, func(tx pgx.Tx) error {
		// Null rather than empty: agent_runs.recommendation is CHECKed against
		// two values and an empty string is neither. Copied out as a plain
		// string so the column sees the same text it always has.
		var recommendation *string
		if run.Recommendation != "" {
			value := string(run.Recommendation)
			recommendation = &value
		}

		if _, err := tx.Exec(ctx, `
			INSERT INTO agent_runs (
			  dispute_id, attempt, model, prompt_fingerprint, tool_surface,
			  outcome, recommendation, letter, cited_evidence, findings,
			  input_tokens, output_tokens, cost_micros, trace, started_at
			) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15)`,
			claim.DisputeID, claim.Attempt, run.Model, run.PromptFingerprint, surface,
			run.Outcome, recommendation, run.Letter, cited, encodedFindings,
			run.Usage.InputTokens, run.Usage.OutputTokens, run.CostMicros,
			encodedTrace, run.StartedAt,
		); err != nil {
			return fmt.Errorf("insert agent run: %w", err)
		}

		tag, err := tx.Exec(ctx, `
			UPDATE disputes SET state = $3, version = version + 1
			 WHERE id = $1 AND version = $2`, claim.DisputeID, claim.Version, toState)
		if err != nil {
			return fmt.Errorf("move dispute %d: %w", claim.DisputeID, err)
		}
		if tag.RowsAffected() == 0 {
			return ErrClaimLost
		}

		return events.Record(ctx, tx, events.Event{
			DisputeID: claim.DisputeID,
			FromState: dispute.StateResolving,
			ToState:   toState,
			Actor:     "agent",
			Detail: map[string]any{
				"attempt":         claim.Attempt,
				"outcome":         run.Outcome,
				"model":           run.Model,
				"cost_micros":     run.CostMicros,
				"findings":        len(findings),
				"escalated":       run.Escalated,
				"awaiting_review": awaitingReview,
			},
		})
	})
}

// nextState maps a run onto where the dispute goes.
//
// Two ways to reach a reviewer. The ordinary one is a run that produced
// something readable. The other is escalation: retrying has stopped working and
// the deadline is still moving, so the last attempt goes in front of a person
// with its findings attached rather than being redrafted until the dispute
// expires.
func nextState(outcome Outcome, escalated bool) (state dispute.State, awaitingReview bool) {
	if escalated {
		return dispute.StateDraftReady, true
	}
	switch outcome {
	case OutcomeDrafted, OutcomeInsufficient:
		return dispute.StateDraftReady, true
	default:
		return dispute.StateReceived, false
	}
}
