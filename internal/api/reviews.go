package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"

	"github.com/regisoliveira/dispute-router/internal/dispute"
	"github.com/regisoliveira/dispute-router/internal/events"
)

// ErrNotReviewable means the run is not in a state a decision can be made
// about: it was already decided, or the dispute moved on underneath it.
var ErrNotReviewable = errors.New("run is not awaiting review")

// ReviewRow is one entry in the queue.
type ReviewRow struct {
	RunID      int64  `json:"run_id"`
	DisputeID  int64  `json:"dispute_id"`
	Reference  string `json:"reference"`
	Merchant   string `json:"merchant"`
	ReasonCode string `json:"reason_code"`
	Amount     Money  `json:"amount"`

	Outcome        string `json:"outcome"`
	Recommendation string `json:"recommendation"`
	Findings       int    `json:"findings"`
	CostMicros     int64  `json:"cost_micros"`
	Attempt        int    `json:"attempt"`

	// SecondsToDeadline is why this queue is ordered the way it is. A draft
	// nobody approved is money running out of time, and the reviewer needs to
	// see that before they see anything else.
	SecondsToDeadline int64     `json:"seconds_to_deadline"`
	FinishedAt        time.Time `json:"finished_at"`
}

// Reviews lists what is waiting.
//
// Ordered by deadline, not by age. The oldest draft is not the most urgent one,
// and a queue sorted by arrival quietly lets the tightest deadline sit behind
// three comfortable ones.
func (s *Store) Reviews(ctx context.Context, limit int) ([]ReviewRow, error) {
	// Clamped down to the cap rather than reset to the default, so that asking
	// for 201 cannot return fewer rows than asking for 200.
	if limit <= 0 {
		limit = defaultLimit
	}
	limit = min(limit, maxLimit)

	// 'draft_ready' is spelled out rather than bound: the review queue's index
	// (disputes_draft_ready_deadline_idx) is partial on exactly that literal,
	// and a bind parameter is not something the planner can prove implies it.
	rows, err := s.pool.Query(ctx, `
		SELECT a.id, d.id, d.external_id, m.name, d.reason_code,
		       d.amount_minor, d.currency,
		       a.outcome, coalesce(a.recommendation, ''),
		       jsonb_array_length(a.findings), a.cost_micros, a.attempt,
		       EXTRACT(EPOCH FROM (d.deadline_at - now()))::bigint,
		       a.finished_at
		  FROM agent_runs a
		  JOIN disputes  d ON d.id = a.dispute_id
		  JOIN merchants m ON m.id = d.merchant_id
		 WHERE a.review IS NULL
		   AND d.state = 'draft_ready'
		 ORDER BY d.deadline_at
		 LIMIT $1`, limit)
	if err != nil {
		return nil, fmt.Errorf("list reviews: %w", err)
	}
	defer rows.Close()

	out := []ReviewRow{}
	for rows.Next() {
		var r ReviewRow
		if err := rows.Scan(&r.RunID, &r.DisputeID, &r.Reference, &r.Merchant, &r.ReasonCode,
			&r.Amount.AmountMinor, &r.Amount.Currency,
			&r.Outcome, &r.Recommendation, &r.Findings, &r.CostMicros, &r.Attempt,
			&r.SecondsToDeadline, &r.FinishedAt); err != nil {
			return nil, fmt.Errorf("scan review: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// ReviewFinding mirrors agent.Finding, redeclared rather than imported so this
// package does not depend on the agent. The rows are read out of JSONB and the
// two shapes are pinned together by a test.
type ReviewFinding struct {
	Check string `json:"check"`
	Quote string `json:"quote"`
	Why   string `json:"why"`
}

// ReviewDetail is everything a reviewer needs on one screen.
type ReviewDetail struct {
	RunID     int64 `json:"run_id"`
	DisputeID int64 `json:"dispute_id"`
	Attempt   int   `json:"attempt"`

	Outcome        string `json:"outcome"`
	Recommendation string `json:"recommendation"`
	Letter         string `json:"letter"`

	CitedEvidence []string        `json:"cited_evidence"`
	Findings      []ReviewFinding `json:"findings"`

	Model             string   `json:"model"`
	PromptFingerprint string   `json:"prompt_fingerprint"`
	ToolSurface       []string `json:"tool_surface"`
	InputTokens       int      `json:"input_tokens"`
	OutputTokens      int      `json:"output_tokens"`
	CostMicros        int64    `json:"cost_micros"`

	StartedAt  time.Time `json:"started_at"`
	FinishedAt time.Time `json:"finished_at"`

	// Decided is populated once somebody has acted, so a run that has already
	// been reviewed reads as history rather than as work.
	Decided   string     `json:"decided,omitempty"`
	DecidedBy string     `json:"decided_by,omitempty"`
	DecidedAt *time.Time `json:"decided_at,omitempty"`

	// The dispute as it stands now, read fresh rather than stored on the run.
	// The record can move between drafting and reviewing - a refund posted, a
	// deadline passing - and a reviewer deciding against a stale snapshot is
	// deciding against something that is no longer true.
	Dispute DisputeDetail `json:"dispute"`
}

// Review loads one run for the review page, with the dispute as it stands now.
func (s *Store) Review(ctx context.Context, runID int64) (ReviewDetail, error) {
	var d ReviewDetail
	var findings []byte
	var decided, decidedBy *string

	err := s.pool.QueryRow(ctx, `
		SELECT a.id, a.dispute_id, a.attempt, a.outcome, coalesce(a.recommendation, ''),
		       a.letter, a.cited_evidence, a.findings,
		       a.model, a.prompt_fingerprint, a.tool_surface,
		       a.input_tokens, a.output_tokens, a.cost_micros,
		       a.started_at, a.finished_at,
		       a.review, a.reviewed_by, a.reviewed_at
		  FROM agent_runs a
		 WHERE a.id = $1`, runID).
		Scan(&d.RunID, &d.DisputeID, &d.Attempt, &d.Outcome, &d.Recommendation,
			&d.Letter, &d.CitedEvidence, &findings,
			&d.Model, &d.PromptFingerprint, &d.ToolSurface,
			&d.InputTokens, &d.OutputTokens, &d.CostMicros,
			&d.StartedAt, &d.FinishedAt,
			&decided, &decidedBy, &d.DecidedAt)

	if errors.Is(err, pgx.ErrNoRows) {
		return ReviewDetail{}, ErrNotFound
	}
	if err != nil {
		return ReviewDetail{}, fmt.Errorf("load run %d: %w", runID, err)
	}

	if err := json.Unmarshal(findings, &d.Findings); err != nil {
		return ReviewDetail{}, fmt.Errorf("decode findings for run %d: %w", runID, err)
	}
	if decided != nil {
		d.Decided = *decided
	}
	if decidedBy != nil {
		d.DecidedBy = *decidedBy
	}

	dispute, err := s.Dispute(ctx, d.DisputeID)
	if err != nil {
		return ReviewDetail{}, fmt.Errorf("load dispute %d for run %d: %w", d.DisputeID, runID, err)
	}
	d.Dispute = dispute

	return d, nil
}

// maxReviewerRunes bounds the name a caller supplies. Until there is a login
// this is free text from an unauthenticated request, and it is written into
// dispute_events.actor - which the agent's record renders. A name is a name:
// one line, printable, short.
const maxReviewerRunes = 64

func validReviewer(reviewer string) error {
	if strings.TrimSpace(reviewer) == "" {
		return fmt.Errorf("%w: a reviewer is required", ErrInvalidInput)
	}
	if utf8.RuneCountInString(reviewer) > maxReviewerRunes {
		return fmt.Errorf("%w: reviewer must be at most %d characters", ErrInvalidInput, maxReviewerRunes)
	}
	for _, r := range reviewer {
		if unicode.IsControl(r) {
			return fmt.Errorf("%w: reviewer must be a single line of printable text", ErrInvalidInput)
		}
	}
	return nil
}

// Decide records a human decision.
//
// Everything else in this package reads. This is the one write, and the only
// code path in the whole system that moves a dispute to 'represented'. The
// agent deliberately cannot reach it - draft_ready is as far as it goes - and
// the worker's rule engine never represents, so this is where the decision to
// send something to a card network actually happens.
//
// Submitted means the letter goes to the card network and the dispute becomes
// 'represented'. Discarded means the draft was not good enough and the dispute
// returns to the queue - it does not mean giving up on the dispute, because
// writing off money is a decision this system never makes on its own and does
// not offer a button for.
//
// One transaction, and the dispute's state is part of the WHERE clause rather
// than something checked beforehand: two reviewers with the page open is the
// ordinary case, not the exotic one.
func (s *Store) Decide(ctx context.Context, runID int64, decision, reviewer string) error {
	var toState dispute.State
	switch decision {
	case "submitted":
		toState = dispute.StateRepresented
	case "discarded":
		toState = dispute.StateReceived
	default:
		return fmt.Errorf("%w: decision must be submitted or discarded, got %q", ErrInvalidInput, decision)
	}

	if err := validReviewer(reviewer); err != nil {
		return err
	}

	return pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		var disputeID int64
		err := tx.QueryRow(ctx, `
			UPDATE agent_runs
			   SET review = $2, reviewed_by = $3, reviewed_at = now()
			 WHERE id = $1 AND review IS NULL
			RETURNING dispute_id`, runID, decision, reviewer).Scan(&disputeID)

		if errors.Is(err, pgx.ErrNoRows) {
			// Either no such run, or somebody already decided. Both are the
			// same answer to the person holding a stale page.
			return ErrNotReviewable
		}
		if err != nil {
			return fmt.Errorf("record decision on run %d: %w", runID, err)
		}

		// 'represented' is terminal in this schema's eyes only for the
		// resolved_at CHECK, which it does not satisfy - so it is left null.
		//
		// The deadline is part of the WHERE clause. A representment sent after
		// the window closed is a letter to a network that has already ruled;
		// the sweeper will record the dispute as expired, and the reviewer is
		// told the page is stale rather than allowed to send into nothing.
		tag, err := tx.Exec(ctx, `
			UPDATE disputes SET state = $2, version = version + 1
			 WHERE id = $1 AND state = $3 AND deadline_at > now()`,
			disputeID, toState, dispute.StateDraftReady)
		if err != nil {
			return fmt.Errorf("move dispute %d: %w", disputeID, err)
		}
		if tag.RowsAffected() == 0 {
			return ErrNotReviewable
		}

		return events.Record(ctx, tx, events.Event{
			DisputeID: disputeID,
			FromState: dispute.StateDraftReady,
			ToState:   toState,
			Actor:     "user:" + reviewer,
			Detail: map[string]any{
				"run":      runID,
				"decision": decision,
				"reviewer": reviewer,
			},
		})
	})
}
