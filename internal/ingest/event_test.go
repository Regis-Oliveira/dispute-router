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
