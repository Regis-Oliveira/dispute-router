package ingest

import (
	"errors"
	"strings"
	"testing"
	"time"
)

var reference = time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)

func validEvent() DisputeWebhook {
	return DisputeWebhook{
		ID:        "evt_1",
		Type:      "dispute.opened",
		CreatedAt: reference,
		Data: DisputeData{
			DisputeID:     "dsp_1",
			MerchantID:    "mrc_northwind",
			TransactionID: "txn_northwind_0000001",
			Kind:          "alert",
			CardNetwork:   "visa",
			ReasonCode:    "10.4",
			AmountMinor:   4999,
			Currency:      "USD",
			OpenedAt:      reference,
			RespondBy:     reference.Add(48 * time.Hour),
		},
	}
}

func TestValidateAcceptsAWellFormedEvent(t *testing.T) {
	if err := validEvent().Validate(reference); err != nil {
		t.Fatalf("Validate() = %v, want nil", err)
	}
}

func TestValidateRejects(t *testing.T) {
	tests := map[string]func(*DisputeWebhook){
		"missing id":            func(e *DisputeWebhook) { e.ID = "" },
		"unsupported type":      func(e *DisputeWebhook) { e.Type = "dispute.closed" },
		"missing dispute id":    func(e *DisputeWebhook) { e.Data.DisputeID = "" },
		"missing merchant":      func(e *DisputeWebhook) { e.Data.MerchantID = "" },
		"missing transaction":   func(e *DisputeWebhook) { e.Data.TransactionID = "" },
		"unknown kind":          func(e *DisputeWebhook) { e.Data.Kind = "retrieval" },
		"unknown network":       func(e *DisputeWebhook) { e.Data.CardNetwork = "jcb" },
		"missing reason":        func(e *DisputeWebhook) { e.Data.ReasonCode = "" },
		"zero amount":           func(e *DisputeWebhook) { e.Data.AmountMinor = 0 },
		"negative amount":       func(e *DisputeWebhook) { e.Data.AmountMinor = -100 },
		"lowercase currency":    func(e *DisputeWebhook) { e.Data.Currency = "usd" },
		"long currency":         func(e *DisputeWebhook) { e.Data.Currency = "USDC" },
		"deadline before open":  func(e *DisputeWebhook) { e.Data.RespondBy = e.Data.OpenedAt.Add(-time.Hour) },
		"deadline already past": func(e *DisputeWebhook) { e.Data.RespondBy = reference.Add(-time.Hour) },
	}

	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			event := validEvent()
			mutate(&event)
			if err := event.Validate(reference); !errors.Is(err, ErrInvalidEvent) {
				t.Fatalf("Validate() = %v, want ErrInvalidEvent", err)
			}
		})
	}
}

func TestDecodeRejectsUnknownFields(t *testing.T) {
	body := []byte(`{"id":"evt_1","type":"dispute.opened","surprise":true}`)
	if _, err := Decode(body); !errors.Is(err, ErrInvalidEvent) {
		t.Fatalf("Decode() = %v, want ErrInvalidEvent", err)
	}
}

func TestDecodeRejectsTrailingContent(t *testing.T) {
	body := []byte(`{"id":"evt_1","type":"dispute.opened"} {"id":"evt_2"}`)
	if _, err := Decode(body); !errors.Is(err, ErrInvalidEvent) {
		t.Fatalf("Decode() = %v, want ErrInvalidEvent", err)
	}
}

// A float amount would silently truncate if it were decoded into an int64 by a
// lenient parser. encoding/json refuses, which is the behaviour worth pinning.
func TestDecodeRejectsFractionalAmount(t *testing.T) {
	body := []byte(`{"id":"evt_1","type":"dispute.opened","created_at":"2026-03-01T12:00:00Z",
		"data":{"dispute_id":"d","merchant_id":"m","transaction_id":"t","kind":"alert",
		"card_network":"visa","reason_code":"10.4","amount_minor":49.99,"currency":"USD",
		"opened_at":"2026-03-01T12:00:00Z","respond_by":"2026-03-03T12:00:00Z"}}`)

	_, err := Decode(body)
	if err == nil {
		t.Fatal("Decode() accepted a fractional amount_minor")
	}
	if !strings.Contains(err.Error(), "amount_minor") {
		t.Fatalf("Decode() = %v, want an error naming amount_minor", err)
	}
}

func validRuling() RulingWebhook {
	return RulingWebhook{
		ID:        "evt_2",
		Type:      TypeDisputeResolved,
		CreatedAt: reference,
		Data: RulingData{
			DisputeID:  "dsp_1",
			MerchantID: "mrc_northwind",
			Outcome:    "won",
			DecidedAt:  reference,
			Note:       "compelling evidence accepted",
		},
	}
}

func TestRulingValidateAcceptsBothOutcomes(t *testing.T) {
	for _, outcome := range []string{"won", "lost"} {
		event := validRuling()
		event.Data.Outcome = outcome
		if err := event.Validate(); err != nil {
			t.Errorf("outcome %q: %v", outcome, err)
		}
	}
}

func TestRulingValidateRejects(t *testing.T) {
	tests := map[string]func(*RulingWebhook){
		"missing id":       func(e *RulingWebhook) { e.ID = "" },
		"wrong type":       func(e *RulingWebhook) { e.Type = TypeDisputeOpened },
		"missing dispute":  func(e *RulingWebhook) { e.Data.DisputeID = "" },
		"missing merchant": func(e *RulingWebhook) { e.Data.MerchantID = "" },
		"no decided_at":    func(e *RulingWebhook) { e.Data.DecidedAt = time.Time{} },
		// There is no third answer. "partial" and "pending" are not outcomes,
		// and accepting one would put a dispute in a state the schema forbids.
		"invented outcome": func(e *RulingWebhook) { e.Data.Outcome = "partial" },
		"empty outcome":    func(e *RulingWebhook) { e.Data.Outcome = "" },
		"cased outcome":    func(e *RulingWebhook) { e.Data.Outcome = "WON" },
	}

	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			event := validRuling()
			mutate(&event)
			if err := event.Validate(); !errors.Is(err, ErrInvalidEvent) {
				t.Fatalf("Validate() = %v, want ErrInvalidEvent", err)
			}
		})
	}
}

// Routing happens before either strict decoder runs, so it has to work on a
// body that neither of them would accept.
func TestPeekTypeRoutesBeforeDecoding(t *testing.T) {
	tests := map[string]string{
		`{"id":"evt_1","type":"dispute.opened","data":{"anything":1}}`:   TypeDisputeOpened,
		`{"id":"evt_2","type":"dispute.resolved","data":{"nonsense":2}}`: TypeDisputeResolved,
		`{"id":"evt_3","type":"dispute.updated"}`:                        "dispute.updated",
	}
	for body, want := range tests {
		_, got, err := PeekType([]byte(body))
		if err != nil {
			t.Errorf("PeekType(%s) = %v", body, err)
			continue
		}
		if got != want {
			t.Errorf("PeekType(%s) = %q, want %q", body, got, want)
		}
	}
}

func TestPeekTypeRejectsWhatItCannotRoute(t *testing.T) {
	for _, body := range []string{`{"id":"evt_1"}`, `not json`, `[]`, ``} {
		if _, _, err := PeekType([]byte(body)); !errors.Is(err, ErrInvalidEvent) {
			t.Errorf("PeekType(%q) = %v, want ErrInvalidEvent", body, err)
		}
	}
}

// A ruling body must not be readable as a dispute, or a malformed one could be
// silently misfiled.
func TestTheDecodersDoNotAcceptEachOthersBodies(t *testing.T) {
	ruling := `{"id":"evt_2","type":"dispute.resolved","created_at":"2026-03-01T12:00:00Z",
		"data":{"dispute_id":"d","merchant_id":"m","outcome":"won",
		"decided_at":"2026-03-01T12:00:00Z","note":"n"}}`

	if _, err := Decode([]byte(ruling)); err == nil {
		t.Error("the dispute decoder accepted a ruling body")
	}

	opened := `{"id":"evt_1","type":"dispute.opened","created_at":"2026-03-01T12:00:00Z",
		"data":{"dispute_id":"d","merchant_id":"m","transaction_id":"t","kind":"alert",
		"card_network":"visa","reason_code":"10.4","amount_minor":100,"currency":"USD",
		"opened_at":"2026-03-01T12:00:00Z","respond_by":"2026-03-03T12:00:00Z"}}`

	if _, err := DecodeRuling([]byte(opened)); err == nil {
		t.Error("the ruling decoder accepted a dispute body")
	}
}
