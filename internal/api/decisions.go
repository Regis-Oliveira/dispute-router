package api

import (
	"context"
	"fmt"
	"time"
)

// Decision is what a reviewer did with a draft.
//
// It is the vocabulary of the agent_runs.review column, whose CHECK constraint
// (agent_runs_review_check, db/migrations/000003_agent.up.sql) allows exactly
// these two values plus NULL for "nobody has decided yet". The strings are the
// database's: a value changed here without a migration is a write Postgres
// refuses. This package is the only writer of that column, which is why the
// type lives here rather than in internal/dispute - a decision is made about a
// run, not about a dispute's own lifecycle.
type Decision string

const (
	// DecisionSubmitted sends the letter to the card network and moves the
	// dispute to 'represented'.
	DecisionSubmitted Decision = "submitted"

	// DecisionDiscarded means the draft was not good enough and the dispute
	// returns to the queue. It does not mean giving up on the dispute; see
	// Store.Decide for why there is no third value.
	DecisionDiscarded Decision = "discarded"
)

// valid reports whether d is one of the two values the CHECK constraint
// accepts, so that a bad one is refused at the edge rather than at the write.
func (d Decision) valid() bool {
	return d == DecisionSubmitted || d == DecisionDiscarded
}

// outcomeRejected is agent.OutcomeRejected, the verifier's verdict that half of
// an override is made of.
//
// Spelled again rather than imported: internal/agent imports this package (for
// api.Store, api.Filters and api.MaxUploadBytes), so the import cannot go the
// other way. Nothing makes the compiler check the two against each other -
// renaming the constant in internal/agent would leave this matching a value
// nothing writes any more, and the query would quietly report no overrides.
// The third copy is agent_runs.outcome's CHECK constraint, which is the only
// one of the three that would refuse a bad write.
const outcomeRejected = "rejected"

// overrideExpr is the pair the decision history exists to show: the verifier
// refused this letter and a person sent it anyway. One format string, because
// it is a column in the SELECT list and sometimes a condition in the WHERE
// clause, and those two must always test the same thing.
const overrideExpr = "(a.outcome = $%d AND a.review = $%d)"

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
	Decision      Decision
	OnlyOverrides bool
	Limit         int
	Offset        int
}

// DecisionList is one page of the decision history.
type DecisionList struct {
	Rows []DecisionRow `json:"rows"`
	Page Page          `json:"page"`
	// Reviewers is every identity that has ever decided, over the whole
	// history rather than the page - a filter built from one page of results
	// hides the people whose decisions are older than fifty rows.
	Reviewers []string `json:"reviewers"`
	TookMS    int64    `json:"took_ms"`
}

// Decisions lists every run a person has acted on.
//
// A different question from the review queue, which is why it is a different
// endpoint rather than a filter on the disputes list. "What did this person
// approve" is a timeline of actions, not a slice of cases - and the things
// that make it worth asking (was the draft rejected, how many findings, what
// did it cost) live on agent_runs, which the disputes list cannot reach.
//
// The column that earns the screen is the pair: the agent's verdict against
// the human's decision. A run the verifier rejected and a person submitted
// anyway is an override, and a list of overrides is the most actionable thing
// this system can produce about itself.
func (s *Store) Decisions(ctx context.Context, f DecisionFilters) (DecisionList, error) {
	started := time.Now()

	// The same clamp Filters.normalize applies, and for the same reason: a
	// limit above the cap comes down to the cap rather than back to the
	// default, so asking for 201 cannot return fewer rows than asking for 200.
	if f.Limit <= 0 {
		f.Limit = defaultLimit
	}
	f.Limit = min(f.Limit, maxLimit)
	f.Offset = max(f.Offset, 0)

	// The same builder the disputes list uses, rather than a second copy of it
	// written as a closure: a condition and the value it tests stay paired, and
	// there is one place where that is true of this package.
	var b builder
	b.conds = append(b.conds, "a.review IS NOT NULL")
	if f.Reviewer != "" {
		b.add("a.reviewed_by = $%d", f.Reviewer)
	}
	if f.Decision != "" {
		b.add("a.review = $%d", f.Decision)
	}
	if f.OnlyOverrides {
		// Bound, not spelled into the statement: no index predicate on
		// agent_runs names either value. The one partial index there
		// (agent_runs_awaiting_review_idx) is on `review IS NULL AND outcome =
		// 'drafted'`, which this expression cannot satisfy anyway - a decided
		// run is exactly what it excludes.
		b.conds = append(b.conds, b.expr(overrideExpr, outcomeRejected, DecisionSubmitted))
	}
	where := b.where()

	var total int64
	if err := s.pool.QueryRow(ctx,
		`SELECT count(*) FROM agent_runs a`+where, b.args...).Scan(&total); err != nil {
		return DecisionList{}, fmt.Errorf("count decisions: %w", err)
	}

	// Copied, not appended in place. append on b.args can write the limit and
	// the offset into the backing array the count query above is still reading
	// from - safe today only because that query had already returned, which is
	// a fact about statement order rather than about this code.
	//
	// This query binds more than the count did: the override pair is a column
	// in its SELECT list whether or not it is also a condition, and the count
	// query's text must not be handed parameters it never mentions.
	rowArgs := builder{args: append([]any{}, b.args...)}
	overrideColumn := rowArgs.expr(overrideExpr, outcomeRejected, DecisionSubmitted)
	page := rowArgs.expr(" LIMIT $%d OFFSET $%d", f.Limit, f.Offset)

	rows, err := s.pool.Query(ctx, `
		SELECT a.id, d.id, d.external_id, m.name, d.amount_minor, d.currency,
		       a.outcome, coalesce(a.recommendation, ''),
		       jsonb_array_length(a.findings), a.attempt, a.cost_micros,
		       a.review, a.reviewed_by, a.reviewed_at,
		       `+overrideColumn+`,
		       d.state
		  FROM agent_runs a
		  JOIN disputes  d ON d.id = a.dispute_id
		  JOIN merchants m ON m.id = d.merchant_id`+where+`
		 ORDER BY a.reviewed_at DESC`+page,
		rowArgs.args...)
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
	out.TookMS = time.Since(started).Milliseconds()
	return out, reviewers.Err()
}
