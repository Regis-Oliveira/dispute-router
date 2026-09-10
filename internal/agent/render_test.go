package agent

import (
	"strings"
	"testing"
)

// The prompt carries no minor units at all. It used to carry the raw record
// with an AMOUNTS block beside it saying which fields not to read, and a rule
// that lives in a warning is a request. Now there is nothing to warn about.
func TestThePromptCarriesFormattedMoneyAndNoMinorUnits(t *testing.T) {
	facts := Facts{}
	facts.Dispute.AmountMinor = 5799
	facts.Dispute.OriginalCharge = 5799
	facts.Dispute.RefundedMinor = 100
	facts.Dispute.Currency = "USD"
	facts.History = nil

	rendered, err := facts.Render()
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	for _, want := range []string{`"amount":"57.99 USD"`, `"original_charge":"57.99 USD"`, `"refunded":"1.00 USD"`} {
		if !strings.Contains(rendered, want) {
			t.Errorf("the record lacks %s:\n%s", want, rendered)
		}
	}
	for _, leak := range []string{"minor", "5799", "AMOUNTS"} {
		if strings.Contains(rendered, leak) {
			t.Errorf("the record still carries %q:\n%s", leak, rendered)
		}
	}
}
