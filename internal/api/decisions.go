package api

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// The decision history: every run a person has acted on.
//
// This is a different question from the review queue, which is why it is a
// different endpoint rather than a filter on the disputes list. "What did this
// person approve" is a timeline of actions, not a slice of cases - and the
// things that make it worth asking (was the draft rejected, how many findings,
// what did it cost) live on agent_runs, which the disputes list cannot reach.
//
// The column that earns the screen is the pair: the agent's verdict against the
// human's decision. A run the verifier rejected and a person submitted anyway
// is an override, and a list of overrides is the most actionable thing this
// system can produce about itself.

// DecisionRow is one decision.
type DecisionRow struct {
	RunID     int64  `json:"run_id"`
	DisputeID int64  `json:"dispute_id"`
	Reference string `json:"reference"`
	Merchant  string `json:"merchant"`
	Amount    Money  `json:"amount"`

	// What the agent concluded.
	Outcome        string `json:"outcome"`
	Recommendation string `json:"recommendation"`
	Findings       int    `json:"findings"`
	Attempt        int    `json:"attempt"`
	CostMicros     int64  `json:"cost_micros"`

	// What the person decided.
	Review     string    `json:"review"`
	ReviewedBy string    `json:"reviewed_by"`
	ReviewedAt time.Time `json:"reviewed_at"`

	// Override is the pair worth looking at: the verifier refused this letter
	// and a person sent it anyway. Computed here rather than left for a client
	// to derive, so every reader of this API agrees on what one is.
	Override bool `json:"override"`

	// DisputeState is where the dispute stands now, which is how a decision
	// eventually gets an outcome: submitted then lost is the interesting shape.
	DisputeState string `json:"dispute_state"`
}

// DecisionFilters narrows the history.
type DecisionFilters struct {
	// Reviewer is matched exactly, never with LIKE or a case fold.
	//
	// It is an identity, not a search term. Today it is a name somebody typed,
	// which is the weakness this system has until it has a login; when it has
	// one this column holds a user id and only the display layer changes.
	// Matching it loosely now would build in the assumption that it is a name,
	// which is the assumption that has to go.
	Reviewer      string
	Decision      string
	OnlyOverrides bool
	Limit         int
	Offset        int
}

type DecisionList struct {
	Rows []DecisionRow `json:"rows"`
	Page Page          `json:"page"`
	// Reviewers is every identity that has ever decided, over the whole
	// history rather than the page - a filter built from one page of results
	// hides the people whose decisions are older than fifty rows.
	Reviewers []string `json:"reviewers"`
	TookMs    int64    `json:"took_ms"`
}

func (s *Store) Decisions(ctx context.Context, f DecisionFilters) (DecisionList, error) {
	started := time.Now()

	if f.Limit <= 0 || f.Limit > 200 {
		f.Limit = 50
	}
	if f.Offset < 0 {
		f.Offset = 0
	}

	var where []string
	var args []any

	add := func(clause string, value any) {
		args = append(args, value)
		where = append(where, fmt.Sprintf(clause, len(args)))
	}

	where = append(where, "a.review IS NOT NULL")
	if f.Reviewer != "" {
		add("a.reviewed_by = $%d", f.Reviewer)
	}
	if f.Decision != "" {
		add("a.review = $%d", f.Decision)
	}
	if f.OnlyOverrides {
		where = append(where, "(a.outcome = 'rejected' AND a.review = 'submitted')")
	}
	clause := strings.Join(where, " AND ")

	var total int64
	if err := s.pool.QueryRow(ctx,
		`SELECT count(*) FROM agent_runs a WHERE `+clause, args...).Scan(&total); err != nil {
		return DecisionList{}, fmt.Errorf("count decisions: %w", err)
	}

	rows, err := s.pool.Query(ctx, `
		SELECT a.id, d.id, d.external_id, m.name, d.amount_minor, d.currency,
		       a.outcome, coalesce(a.recommendation, ''),
		       jsonb_array_length(a.findings), a.attempt, a.cost_micros,
		       a.review, a.reviewed_by, a.reviewed_at,
		       (a.outcome = 'rejected' AND a.review = 'submitted'),
		       d.state
		  FROM agent_runs a
		  JOIN disputes  d ON d.id = a.dispute_id
		  JOIN merchants m ON m.id = d.merchant_id
		 WHERE `+clause+`
		 ORDER BY a.reviewed_at DESC
		 LIMIT $`+fmt.Sprint(len(args)+1)+` OFFSET $`+fmt.Sprint(len(args)+2),
		append(args, f.Limit, f.Offset)...)
	if err != nil {
		return DecisionList{}, fmt.Errorf("list decisions: %w", err)
	}
	defer rows.Close()

	out := DecisionList{Rows: []DecisionRow{}, Reviewers: []string{}}
	for rows.Next() {
		var r DecisionRow
		if err := rows.Scan(&r.RunID, &r.DisputeID, &r.Reference, &r.Merchant,
			&r.Amount.AmountMinor, &r.Amount.Currency,
			&r.Outcome, &r.Recommendation, &r.Findings, &r.Attempt, &r.CostMicros,
			&r.Review, &r.ReviewedBy, &r.ReviewedAt, &r.Override, &r.DisputeState); err != nil {
			return DecisionList{}, fmt.Errorf("scan decision: %w", err)
		}
		out.Rows = append(out.Rows, r)
	}
	if err := rows.Err(); err != nil {
		return DecisionList{}, err
	}

	// Unfiltered on purpose: the list of people who can be filtered for must
	// not depend on the filter currently applied, or selecting one reviewer
	// removes everybody else from the control that selected them.
	reviewers, err := s.pool.Query(ctx,
		`SELECT DISTINCT reviewed_by FROM agent_runs WHERE reviewed_by IS NOT NULL ORDER BY 1`)
	if err != nil {
		return DecisionList{}, fmt.Errorf("list reviewers: %w", err)
	}
	defer reviewers.Close()
	for reviewers.Next() {
		var who string
		if err := reviewers.Scan(&who); err != nil {
			return DecisionList{}, fmt.Errorf("scan reviewer: %w", err)
		}
		out.Reviewers = append(out.Reviewers, who)
	}

	out.Page = Page{Offset: f.Offset, Limit: f.Limit, Total: total}
	out.TookMs = time.Since(started).Milliseconds()
	return out, reviewers.Err()
}
