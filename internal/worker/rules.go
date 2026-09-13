// Package worker drains the deadline queue and decides what to do with each
// dispute before its clock runs out.
package worker

import "time"

// Action is what the worker decided to do.
type Action string

const (
	// ActionRefund gives the money back and closes the dispute. Only ever
	// applied to alerts, where refunding inside the window means no chargeback
	// is filed at all - no fee, no ratio damage.
	ActionRefund Action = "refund"

	// ActionClose ends an alert whose money has already been returned. It
	// moves the dispute to refunded and moves nothing else: the refund that
	// answers this alert was posted by an earlier dispute on the same charge,
	// and the state records that fact rather than repeating the payment.
	ActionClose Action = "close"

	// ActionEscalate is the worker declining to decide. No state change, no
	// money moved: the dispute stays where it is with its clock running and a
	// human picks it up.
	ActionEscalate Action = "escalate"

	// ActionExpire records that the window closed with nobody acting. It is a
	// failure being written down, not an outcome being chosen.
	ActionExpire Action = "expire"

	// ActionSkip means the dispute is no longer the worker's to touch - it was
	// resolved by something else between being scheduled and being claimed.
	ActionSkip Action = "skip"
)

// Decision is what the worker will do to one dispute and why.
type Decision struct {
	Action Action
	// ToState is empty for actions that change nothing.
	ToState string
	// Reason is written into the audit trail. Every automated decision has to
	// be explainable a year later, to someone who was not here.
	Reason string
}

// reasonCategory classifies a network reason code.
//
// The category drives whether a chargeback is worth fighting, so it is worth
// stating explicitly rather than inferring: a card-absent fraud claim is a
// different problem from "the parcel never arrived", and evidence that wins one
// is irrelevant to the other. Mirrors the catalog in the simulator.
type reasonCategory string

const (
	categoryFraud      reasonCategory = "fraud"
	categoryService    reasonCategory = "service"
	categoryProcessing reasonCategory = "processing"
	categoryUnknown    reasonCategory = "unknown"
)

var reasonCategories = map[string]reasonCategory{
	// Visa
	"10.4": categoryFraud,
	"13.1": categoryService, "13.3": categoryService,
	"13.6": categoryService, "13.7": categoryService,
	"12.5": categoryProcessing, "12.6.1": categoryProcessing,
	// Mastercard
	"4837": categoryFraud,
	"4853": categoryService, "4855": categoryService, "4841": categoryService,
	"4834": categoryProcessing,
	// Amex
	"F29": categoryFraud,
	"C08": categoryService, "C31": categoryService, "C02": categoryService,
	"P08": categoryProcessing,
	// Discover
	"UA02": categoryFraud, "UA01": categoryFraud,
	"RG": categoryService, "RM": categoryService, "AP": categoryService,
	"DP": categoryProcessing,
}

func categoryOf(reasonCode string) reasonCategory {
	if category, ok := reasonCategories[reasonCode]; ok {
		return category
	}
	// An unrecognised code is not a licence to guess. It escalates.
	return categoryUnknown
}

// Candidate is everything the decision needs. Deliberately a plain struct with
// no database or Redis in it, so the rules below can be tested exhaustively
// without either.
type Candidate struct {
	ID          int64
	Kind        string // alert | chargeback
	State       string
	ReasonCode  string
	AmountMinor int64
	Currency    string
	DeadlineAt  time.Time
	Resolved    bool

	// AutoRefundCeilingMinor is the merchant's own limit. nil means the
	// merchant has not switched auto-refunding on, and nothing is automatic.
	AutoRefundCeilingMinor *int64

	// RefundableRemainingMinor is the original charge minus everything already
	// given back on it. Several disputes can point at one transaction - a
	// partial dispute, then another - and their refunds share this budget.
	RefundableRemainingMinor int64
}

// Decide is the whole policy, and it is a pure function on purpose: what the
// system does with someone's money is the part that most needs to be readable,
// reviewable, and testable without standing up a database.
//
// What this policy deliberately does not do: it never automatically concedes a
// chargeback, and it never represents one either - the only path to
// 'represented' is a person approving a draft. Auto-refunding an alert is
// strictly cheaper than the alternative, so it is safe to automate; giving up
// on a chargeback is a judgement about evidence and about a merchant
// relationship, and a rule engine that quietly writes off money is the one
// nobody notices is wrong.
func Decide(c Candidate, now time.Time) Decision {
	if c.Resolved || (c.State != "received" && c.State != "resolving" && c.State != "draft_ready") {
		return Decision{Action: ActionSkip, Reason: "already resolved"}
	}

	// The clock is checked before anything else. Past the deadline every other
	// rule is describing an option that no longer exists - and that includes a
	// draft nobody approved. draft_ready used to be invisible to this sweeper,
	// so a dispute waiting on a reviewer who went on holiday sat past its
	// deadline with its funds still held and a book that balanced and lied.
	if !now.Before(c.DeadlineAt) {
		return Decision{
			Action:  ActionExpire,
			ToState: "expired",
			Reason:  "deadline passed with no decision",
		}
	}

	// A draft with time left belongs to a person. The worker only ever
	// touches it once the clock has run out.
	if c.State == "draft_ready" {
		return Decision{Action: ActionSkip, Reason: "awaiting a reviewer"}
	}

	switch c.Kind {
	case "alert":
		// An alert on a charge that has already been refunded in full is
		// answered, and the answer is "already refunded". Nothing is left to
		// give back, so the merchant's ceiling does not enter into it: no money
		// moves, and the only decision is to write the fact down and close
		// the alert. This used to escalate under the balance rule below, which
		// put twenty-two of them in front of a person with a reason that read
		// like a data fault, to be looked at again just after the deadline
		// and expired - "a failure written down" - for a refund that had
		// already happened.
		if c.RefundableRemainingMinor <= 0 {
			return Decision{
				Action:  ActionClose,
				ToState: "refunded",
				Reason:  "original charge already refunded in full; nothing left to return and no money moved",
			}
		}

		// An alert is the cheap window. Refunding inside it costs the sale;
		// letting it lapse costs the sale, a fee, and a mark against the
		// merchant's chargeback ratio. Below the merchant's own ceiling the
		// arithmetic is not close.
		if c.AutoRefundCeilingMinor == nil {
			return Decision{
				Action: ActionEscalate,
				Reason: "merchant has no auto-refund ceiling configured",
			}
		}
		if c.AmountMinor > *c.AutoRefundCeilingMinor {
			return Decision{
				Action: ActionEscalate,
				Reason: "alert above the merchant's auto-refund ceiling",
			}
		}

		// Refunding more than is left on the original charge is not a thing
		// that can happen, so this is a question about the data rather than
		// about the dispute: part of the charge was refunded elsewhere, and
		// this alert claims more than the rest. The database's
		// CHECK (refunded_minor <= amount_minor) would refuse the write, but
		// arriving there means the worker retries a permanent condition
		// forever. It is a decision, so it is decided here. The full-refund
		// case was decided above; what reaches this rule is a partial one,
		// which is a question for a person.
		if c.AmountMinor > c.RefundableRemainingMinor {
			return Decision{
				Action: ActionEscalate,
				Reason: "refund would exceed what is left refundable on the original charge",
			}
		}

		return Decision{
			Action:  ActionRefund,
			ToState: "refunded",
			Reason:  "alert within the merchant's auto-refund ceiling",
		}

	case "chargeback":
		// The money is already gone; the only question is whether it is worth
		// arguing for, and arguing means writing a letter. This rule engine
		// used to move evidence-led chargebacks straight to 'represented' with
		// no letter and no person - a state that means "evidence submitted"
		// reached with nothing submitted. Now nothing here represents: the
		// assistant drafts, a person submits, and the worker's part is to
		// leave the dispute where the assistant will find it and to expire it
		// if nobody does. The category still matters, because it is the
		// reason written into the audit trail.
		//
		// A chargeback on a charge already refunded in full is the one case
		// where the letter writes itself - "credit already issued" is the
		// representment - but it is still a letter, so it is still a person's.
		// The reason says what the person will find.
		if c.RefundableRemainingMinor <= 0 {
			return Decision{
				Action: ActionEscalate,
				Reason: "original charge already refunded in full; a credit-issued representment needs a person",
			}
		}
		switch categoryOf(c.ReasonCode) {
		case categoryService, categoryProcessing:
			return Decision{
				Action: ActionEscalate,
				Reason: "evidence-led reason code; the assistant drafts and a person submits",
			}
		case categoryFraud:
			return Decision{
				Action: ActionEscalate,
				Reason: "fraud reason code; representment rarely wins without 3DS evidence",
			}
		default:
			return Decision{
				Action: ActionEscalate,
				Reason: "unrecognised reason code " + c.ReasonCode,
			}
		}
	}

	return Decision{Action: ActionEscalate, Reason: "unknown dispute kind " + c.Kind}
}
