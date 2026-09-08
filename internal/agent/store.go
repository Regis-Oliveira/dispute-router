package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ErrClaimLost means another process moved the dispute between the read and
// the write. Not a failure: the version check did its job, the same way it does
// for the worker.
var ErrClaimLost = errors.New("agent: dispute changed under the claim")

// Runs is the agent's persistence: what it is allowed to work on, and what it
// wrote down afterwards.
type Runs struct{ pool *pgxpool.Pool }

func NewRuns(pool *pgxpool.Pool) *Runs { return &Runs{pool: pool} }

// Claim is one dispute, held.
type Claim struct {
	DisputeID int64
	Version   int32
	Attempt   int
}

// Candidates lists disputes worth drafting for.
//
// Chargebacks only: an alert is cheaper to refund than to fight, which is
// already the rule in internal/worker/rules.go, and drafting a representment
// for one would be spending model tokens to argue against money that has not
// been taken yet.
//
// The attempt ceiling is what stops a dispute the verifier keeps rejecting from
// being redrafted forever. Past it, the last run goes in front of a person -
// escalation here means a human, because there is nothing else for it to mean.
func (r *Runs) Candidates(ctx context.Context, maxAttempts, limit int) ([]int64, error) {
	rows, err := r.pool.Query(ctx, `
		SELECT d.id
		  FROM disputes d
		 WHERE d.state = 'received'
		   AND d.kind  = 'chargeback'
		   AND d.deadline_at > now()
		   AND (SELECT count(*) FROM agent_runs a WHERE a.dispute_id = d.id) < $1
		 ORDER BY d.deadline_at
		 LIMIT $2`, maxAttempts, limit)
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
	err := r.pool.QueryRow(ctx, `
		WITH held AS (
		  UPDATE disputes
		     SET state = 'resolving', version = version + 1
		   WHERE id = $1 AND state = 'received'
		  RETURNING id, version
		)
		SELECT h.id, h.version,
		       (SELECT count(*) FROM agent_runs a WHERE a.dispute_id = h.id) + 1
		  FROM held h`, disputeID).
		Scan(&claim.DisputeID, &claim.Version, &claim.Attempt)

	if errors.Is(err, pgx.ErrNoRows) {
		return Claim{}, ErrClaimLost
	}
	if err != nil {
		return Claim{}, fmt.Errorf("hold dispute %d: %w", disputeID, err)
	}
	return claim, nil
}

// Release puts a dispute back when the run produced nothing worth reviewing.
func (r *Runs) Release(ctx context.Context, claim Claim) error {
	tag, err := r.pool.Exec(ctx, `
		UPDATE disputes SET state = 'received', version = version + 1
		 WHERE id = $1 AND version = $2`, claim.DisputeID, claim.Version)
	if err != nil {
		return fmt.Errorf("release dispute %d: %w", claim.DisputeID, err)
	}
	if tag.RowsAffected() == 0 {
		return ErrClaimLost
	}
	return nil
}

// Outcome mirrors the CHECK on agent_runs.outcome.
const (
	OutcomeDrafted      = "drafted"
	OutcomeRejected     = "rejected"
	OutcomeInsufficient = "insufficient_evidence"
	OutcomeBudget       = "budget_exceeded"
	OutcomeFailed       = "failed"
)

// Run is everything one attempt produced.
type Run struct {
	Model             string
	PromptFingerprint string
	ToolSurface       []string
	Outcome           string
	Recommendation    string
	Letter            string
	CitedEvidence     []string
	Findings          []Finding
	Usage             Usage
	CostMicros        int64
	Trace             any
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
		var recommendation *string
		if run.Recommendation != "" {
			recommendation = &run.Recommendation
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

		detail, err := json.Marshal(map[string]any{
			"attempt":         claim.Attempt,
			"outcome":         run.Outcome,
			"model":           run.Model,
			"cost_micros":     run.CostMicros,
			"findings":        len(findings),
			"escalated":       run.Escalated,
			"awaiting_review": awaitingReview,
		})
		if err != nil {
			return fmt.Errorf("encode event detail: %w", err)
		}

		if _, err := tx.Exec(ctx, `
			INSERT INTO dispute_events (dispute_id, from_state, to_state, actor, detail)
			VALUES ($1, 'resolving', $2, 'agent', $3)`,
			claim.DisputeID, toState, detail,
		); err != nil {
			return fmt.Errorf("insert dispute event: %w", err)
		}
		return nil
	})
}

// nextState maps a run onto where the dispute goes.
//
// Two ways to reach a reviewer. The ordinary one is a run that produced
// something readable. The other is escalation: retrying has stopped working and
// the deadline is still moving, so the last attempt goes in front of a person
// with its findings attached rather than being redrafted until the dispute
// expires.
func nextState(outcome string, escalated bool) (state string, awaitingReview bool) {
	if escalated {
		return "draft_ready", true
	}
	switch outcome {
	case OutcomeDrafted, OutcomeInsufficient:
		return "draft_ready", true
	default:
		return "received", false
	}
}
