package agent

import (
	"strings"
	"testing"
)

// The bug this file exists for: a letter that said $579.99 about a $57.99
// dispute, because the record handed the model 5799 and the prompt told it to
// copy the record's digits.
func TestFormatMinorDividesExactlyOnce(t *testing.T) {
	cases := map[string]struct {
		minor    int64
		currency string
		want     string
	}{
		"the dispute that failed":   {5799, "USD", "57.99 USD"},
		"the other one that failed": {2790, "USD", "27.90 USD"},
		"zero":                      {0, "USD", "0.00 USD"},
		"a single minor unit":       {1, "USD", "0.01 USD"},
		"grouped":                   {1234567, "EUR", "12,345.67 EUR"},
		"lowercase currency":        {5799, "usd", "57.99 USD"},
	}
	for name, c := range cases {
		if got := FormatMinor(c.minor, c.currency); got != c.want {
			t.Errorf("%s: FormatMinor(%d, %q) = %q, want %q", name, c.minor, c.currency, got, c.want)
		}
	}
}

// Not every currency has cents. A blanket divide by 100 turns 5,000 yen into
// 50, and nothing about the result looks wrong.
func TestZeroDecimalCurrenciesAreNotDivided(t *testing.T) {
	if got := FormatMinor(5000, "JPY"); got != "5,000 JPY" {
		t.Errorf("FormatMinor(5000, JPY) = %q", got)
	}
	if got := FormatMinor(35040, "JPY"); got != "35,040 JPY" {
		t.Errorf("FormatMinor(35040, JPY) = %q", got)
	}
	// And a currency nobody has heard of falls back to two, which is wrong less
	// often than any other guess.
	if got := FormatMinor(100, "XYZ"); got != "1.00 XYZ" {
		t.Errorf("FormatMinor(100, XYZ) = %q", got)
	}
}

// The record hands the model minor units; the prompt must not.
func TestTheAmountsBlockCarriesFormattedMoney(t *testing.T) {
	facts := Facts{}
	facts.Dispute.AmountMinor = 5799
	facts.Dispute.OriginalCharge = 5799
	facts.Dispute.Currency = "USD"

	rendered, err := facts.Render()
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if !strings.Contains(rendered, "57.99 USD") {
		t.Error("the formatted amount is missing from the prompt")
	}
	if !strings.Contains(rendered, "do no arithmetic of your own") {
		t.Error("the prompt does not tell the model to stop converting")
	}
}
