// Package events appends to the dispute audit trail.
//
// One place knows the shape of a dispute_events row. Before this existed four
// packages wrote their own INSERT and the four had already drifted: one omitted
// occurred_at, one cast the detail to jsonb and one did not, and each worded
// its error differently. None of that was a decision; it was four people
// solving the same problem on four different days.
//
// Record takes a pgx.Tx rather than a pool, like ledger.Hold beside it, because
// an audit event that can be written outside the transaction that caused it is
// an audit event that can disagree with the record it describes.
package events

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/regisoliveira/dispute-router/internal/dispute"
)

// Event is one row of the dispute audit trail.
type Event struct {
	DisputeID int64

	// FromState is the state the dispute is leaving. Empty writes NULL, which
	// is what the first event of a dispute means: there was nothing before it.
	FromState dispute.State

	// ToState is the state the dispute is entering.
	ToState dispute.State

	// Actor is who caused the change: "system", "agent", "network",
	// "worker:<id>" or "user:<name>". It is rendered into the record the agent
	// reads, so anything taken from a request is bounded before it gets here.
	Actor string

	// Detail is encoded to JSON by Record rather than by the callers. A
	// hand-built literal with %q is Go quoting, not JSON escaping, and the two
	// disagree on exactly the characters that arrive from outside.
	Detail any

	// OccurredAt is when the change happened, which is not always when it was
	// written down: a webhook carries the processor's own timestamp, and the
	// audit trail is read in that order. The zero value means now(), the same
	// value the column's DEFAULT would have supplied.
	OccurredAt time.Time
}

// Record appends one event to the dispute audit trail inside tx.
func Record(ctx context.Context, tx pgx.Tx, e Event) error {
	detail, err := json.Marshal(e.Detail)
	if err != nil {
		return fmt.Errorf("dispute %d event: encode detail: %w", e.DisputeID, err)
	}

	var fromState *dispute.State
	if e.FromState != "" {
		fromState = &e.FromState
	}
	var occurredAt *time.Time
	if !e.OccurredAt.IsZero() {
		occurredAt = &e.OccurredAt
	}

	if _, err := tx.Exec(ctx, `
		INSERT INTO dispute_events (dispute_id, from_state, to_state, actor, detail, occurred_at)
		VALUES ($1::bigint, $2::text, $3::text, $4::text, $5::jsonb, coalesce($6::timestamptz, now()))`,
		e.DisputeID, fromState, e.ToState, e.Actor, string(detail), occurredAt,
	); err != nil {
		return fmt.Errorf("insert dispute_event for dispute %d: %w", e.DisputeID, err)
	}
	return nil
}
