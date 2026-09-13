// Package dispute holds the vocabulary of the dispute lifecycle: the states a
// dispute moves through and the two kinds it can be.
//
// The strings are the database's, not this package's. disputes.state and
// disputes.kind each carry a CHECK constraint (db/migrations/000001_init.up.sql,
// widened for draft_ready in 000003_agent.up.sql) naming exactly these values,
// so a constant added here without a migration is a write Postgres refuses at
// runtime, and a value changed here without one silently stops matching rows
// already on disk.
//
// Three partial indexes are defined on state literals, which is why a handful
// of queries still spell a state out instead of binding it: the planner can
// only use `... WHERE state IN ('received','resolving')` when it can prove the
// predicate holds, and it cannot prove that about a bind parameter. Those are
// the only SQL literals left; every other query passes a constant from here as
// an argument.
package dispute

// State is where a dispute stands. The zero value is not a state: a dispute
// row always has one, and an empty State in an event means "there was nothing
// before this".
type State string

const (
	// StateReceived is a dispute that has been ingested and not yet acted on.
	// It is also where a discarded draft or an abandoned agent run returns to.
	StateReceived State = "received"

	// StateResolving means somebody holds the dispute and is deciding: a
	// worker with the Redis lock, or the agent with its claim.
	StateResolving State = "resolving"

	// StateDraftReady means the agent wrote a representment and a person has
	// not approved it. Deliberately not terminal: nothing the agent produces
	// resolves a dispute.
	StateDraftReady State = "draft_ready"

	// StateRepresented means evidence was submitted and the network has not
	// ruled. The only path here is a human approving a draft (api.Store.Decide),
	// and it is the one open state with no resolved_at.
	StateRepresented State = "represented"

	// StateRefunded means the money was returned and the dispute is closed.
	StateRefunded State = "refunded"

	// StateWon means the network ruled for the merchant.
	StateWon State = "won"

	// StateLost means the network ruled against the merchant.
	StateLost State = "lost"

	// StateExpired means the window closed with nobody acting. A failure being
	// written down, not an outcome being chosen.
	StateExpired State = "expired"
)

// Kind separates the two things a processor sends. An alert is a warning that
// can be answered with a refund before any money moves; a chargeback is a
// clawback that has already happened. Every rule that spends money branches on
// this, which is why it is a type and not a comment.
type Kind string

const (
	// KindAlert is a pre-chargeback warning: refunding inside the window means
	// no chargeback is filed at all.
	KindAlert Kind = "alert"

	// KindChargeback is a filed chargeback. The funds are already held and the
	// only question is whether to argue.
	KindChargeback Kind = "chargeback"
)
