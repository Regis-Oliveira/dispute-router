package ingest

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"time"
	"unicode/utf8"
)

// ErrInvalidEvent wraps every validation failure so the handler can answer 400
// without inspecting the specific reason.
var ErrInvalidEvent = errors.New("invalid event")

// DisputeWebhook is the processor's message. It mirrors the DisputeWebhook
// interface in services/simulator/src/emit.ts.
type DisputeWebhook struct {
	ID        string      `json:"id"`
	Type      string      `json:"type"`
	CreatedAt time.Time   `json:"created_at"`
	Data      DisputeData `json:"data"`
}

type DisputeData struct {
	DisputeID     string `json:"dispute_id"`
	MerchantID    string `json:"merchant_id"`
	TransactionID string `json:"transaction_id"`
	Kind          string `json:"kind"`
	CardNetwork   string `json:"card_network"`
	ReasonCode    string `json:"reason_code"`

	// Minor units. Decoded as int64 rather than a float so a JSON number too
	// large or too precise for a float64 is a parse error instead of a silently
	// rounded amount.
	AmountMinor int64  `json:"amount_minor"`
	Currency    string `json:"currency"`

	OpenedAt  time.Time `json:"opened_at"`
	RespondBy time.Time `json:"respond_by"`

	// CardholderClaim is the cardholder's own account, when the processor
	// relays one. The only free text on the webhook and the only field written
	// by the party trying to reverse the charge. Stored raw - the record has to
	// say what was claimed - and quarantined where it is read. Bounded here,
	// because a prompt is paid for by the token and the database should not
	// hold a novel.
	CardholderClaim string `json:"cardholder_claim,omitempty"`
}

// MaxClaimBytes bounds the cardholder's claim at the door. Four thousand bytes
// is several paragraphs - longer than any claim an issuer relays - and short
// enough that the prompt boundary's own cut (internal/agent) is rarely reached.
const MaxClaimBytes = 4000

var (
	validKinds    = map[string]bool{"alert": true, "chargeback": true}
	validNetworks = map[string]bool{"visa": true, "mastercard": true, "amex": true, "discover": true}
)

// Decode parses a raw body strictly: unknown fields are an error rather than a
// shrug. A processor that starts sending a field this service silently drops is
// a change worth noticing at the boundary, not three phases later.
func Decode(body []byte) (DisputeWebhook, error) {
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()

	var event DisputeWebhook
	if err := decoder.Decode(&event); err != nil {
		return DisputeWebhook{}, fmt.Errorf("%w: %s", ErrInvalidEvent, err)
	}
	if decoder.More() {
		return DisputeWebhook{}, fmt.Errorf("%w: trailing content after the JSON object", ErrInvalidEvent)
	}
	return event, nil
}

// Validate rejects anything that would put nonsense in the database. Every rule
// here has a matching CHECK constraint in the schema; this layer exists to turn
// a constraint violation into a 400 with a readable reason.
func (e DisputeWebhook) Validate(now time.Time) error {
	if e.ID == "" {
		return fmt.Errorf("%w: id is required", ErrInvalidEvent)
	}
	if e.Type != "dispute.opened" {
		return fmt.Errorf("%w: unsupported type %q", ErrInvalidEvent, e.Type)
	}

	d := e.Data
	switch {
	case d.DisputeID == "":
		return fmt.Errorf("%w: data.dispute_id is required", ErrInvalidEvent)
	case d.MerchantID == "":
		return fmt.Errorf("%w: data.merchant_id is required", ErrInvalidEvent)
	case d.TransactionID == "":
		return fmt.Errorf("%w: data.transaction_id is required", ErrInvalidEvent)
	case !validKinds[d.Kind]:
		return fmt.Errorf("%w: unknown kind %q", ErrInvalidEvent, d.Kind)
	case !validNetworks[d.CardNetwork]:
		return fmt.Errorf("%w: unknown card_network %q", ErrInvalidEvent, d.CardNetwork)
	case d.ReasonCode == "":
		return fmt.Errorf("%w: data.reason_code is required", ErrInvalidEvent)
	case d.AmountMinor <= 0:
		// Zero is not a dispute and a negative amount is a bug upstream. Either
		// way, refusing it here is cheaper than reconciling it later.
		return fmt.Errorf("%w: amount_minor must be positive, got %d", ErrInvalidEvent, d.AmountMinor)
	case !isCurrencyCode(d.Currency):
		return fmt.Errorf("%w: currency must be a 3-letter code, got %q", ErrInvalidEvent, d.Currency)
	case d.OpenedAt.IsZero():
		return fmt.Errorf("%w: data.opened_at is required", ErrInvalidEvent)
	case d.RespondBy.IsZero():
		return fmt.Errorf("%w: data.respond_by is required", ErrInvalidEvent)
	case !d.RespondBy.After(d.OpenedAt):
		return fmt.Errorf("%w: respond_by must be after opened_at", ErrInvalidEvent)
	case d.RespondBy.Before(now):
		// A deadline already in the past cannot be raced. Taking it would put a
		// dispute in the queue that the worker can only ever mark expired.
		return fmt.Errorf("%w: respond_by is already in the past", ErrInvalidEvent)
	case len(d.CardholderClaim) > MaxClaimBytes:
		return fmt.Errorf("%w: cardholder_claim exceeds %d bytes", ErrInvalidEvent, MaxClaimBytes)
	case !utf8.ValidString(d.CardholderClaim):
		return fmt.Errorf("%w: cardholder_claim is not valid UTF-8", ErrInvalidEvent)
	}
	return nil
}

func isCurrencyCode(code string) bool {
	if len(code) != 3 {
		return false
	}
	for _, r := range code {
		if r < 'A' || r > 'Z' {
			return false
		}
	}
	return true
}

// RulingWebhook is the network coming back with a verdict on a representment.
//
// It is what makes "represented" a state rather than a dead end: the merchant
// submitted evidence, weeks passed, and Visa or Mastercard decided.
type RulingWebhook struct {
	ID        string     `json:"id"`
	Type      string     `json:"type"`
	CreatedAt time.Time  `json:"created_at"`
	Data      RulingData `json:"data"`
}

type RulingData struct {
	DisputeID  string `json:"dispute_id"`
	MerchantID string `json:"merchant_id"`
	// Outcome is won or lost. There is no third answer.
	Outcome   string    `json:"outcome"`
	DecidedAt time.Time `json:"decided_at"`
	// Note is the network's reason, kept for the audit trail rather than acted on.
	Note string `json:"note"`
}

const (
	TypeDisputeOpened   = "dispute.opened"
	TypeDisputeResolved = "dispute.resolved"
)

var validOutcomes = map[string]bool{"won": true, "lost": true}

// PeekType reads just enough of a body to route it.
//
// Deliberately lenient where the typed decoders are strict: this only has to
// answer "which decoder", and a body it cannot classify is rejected by that
// decoder with a message about the actual problem.
func PeekType(body []byte) (id string, eventType string, err error) {
	var peek struct {
		ID   string `json:"id"`
		Type string `json:"type"`
	}
	if err := json.Unmarshal(body, &peek); err != nil {
		return "", "", fmt.Errorf("%w: %s", ErrInvalidEvent, err)
	}
	if peek.Type == "" {
		return "", "", fmt.Errorf("%w: type is required", ErrInvalidEvent)
	}
	return peek.ID, peek.Type, nil
}

func DecodeRuling(body []byte) (RulingWebhook, error) {
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()

	var event RulingWebhook
	if err := decoder.Decode(&event); err != nil {
		return RulingWebhook{}, fmt.Errorf("%w: %s", ErrInvalidEvent, err)
	}
	if decoder.More() {
		return RulingWebhook{}, fmt.Errorf("%w: trailing content after the JSON object", ErrInvalidEvent)
	}
	return event, nil
}

func (e RulingWebhook) Validate() error {
	switch {
	case e.ID == "":
		return fmt.Errorf("%w: id is required", ErrInvalidEvent)
	case e.Type != TypeDisputeResolved:
		return fmt.Errorf("%w: unsupported type %q", ErrInvalidEvent, e.Type)
	case e.Data.DisputeID == "":
		return fmt.Errorf("%w: data.dispute_id is required", ErrInvalidEvent)
	case e.Data.MerchantID == "":
		return fmt.Errorf("%w: data.merchant_id is required", ErrInvalidEvent)
	case !validOutcomes[e.Data.Outcome]:
		return fmt.Errorf("%w: outcome must be won or lost, got %q", ErrInvalidEvent, e.Data.Outcome)
	case e.Data.DecidedAt.IsZero():
		return fmt.Errorf("%w: data.decided_at is required", ErrInvalidEvent)
	}
	return nil
}
