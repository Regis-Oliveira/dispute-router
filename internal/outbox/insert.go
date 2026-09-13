package outbox

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// Entry is one row on its way into the outbox.
//
// The mirror of Message, which is the same row on its way out. Both live here
// on purpose: the columns the relay selects and the columns the writers fill
// are one contract, and a contract split across packages is a contract that
// drifts.
type Entry struct {
	// AggregateType names what the event is about; every writer so far says
	// "dispute".
	AggregateType string
	AggregateID   int64

	// EventType is what the consumers switch on, "dispute.received" and the
	// like.
	EventType string

	// Payload is encoded to JSON by Insert, for the same reason ledger and
	// events encode theirs: %q is Go quoting, not JSON escaping.
	Payload any
}

// Insert appends one entry to the outbox inside tx.
//
// tx rather than a pool, and that is the whole pattern: the message commits
// with the change it announces. A publish to SQS cannot be part of a database
// transaction, so the relay reads committed rows instead - which is what makes
// "the dispute exists but the queue never heard about it" impossible.
func Insert(ctx context.Context, tx pgx.Tx, e Entry) error {
	payload, err := json.Marshal(e.Payload)
	if err != nil {
		return fmt.Errorf("marshal outbox payload for %s %d: %w", e.AggregateType, e.AggregateID, err)
	}

	if _, err := tx.Exec(ctx, `
		INSERT INTO outbox (aggregate_type, aggregate_id, event_type, payload)
		VALUES ($1::text, $2::bigint, $3::text, $4::jsonb)`,
		e.AggregateType, e.AggregateID, e.EventType, string(payload),
	); err != nil {
		return fmt.Errorf("insert outbox %s for %s %d: %w", e.EventType, e.AggregateType, e.AggregateID, err)
	}
	return nil
}
