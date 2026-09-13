package money

import "testing"

// The bug this package exists for: a letter that said $579.99 about a $57.99
// dispute, because the record handed the model 5799 and the prompt told it to
// copy the record's digits.
func TestFormatMinorDividesExactlyOnce(t *testing.T) {
	t.Parallel()

	for name, c := range map[string]struct {
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
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if got := FormatMinor(c.minor, c.currency); got != c.want {
				t.Errorf("FormatMinor(%d, %q) = %q, want %q", c.minor, c.currency, got, c.want)
			}
		})
	}
}

// Not every currency has cents. A blanket divide by 100 turns 5,000 yen into
// 50, and nothing about the result looks wrong.
func TestZeroDecimalCurrenciesAreNotDivided(t *testing.T) {
	t.Parallel()

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
	if Digits("jpy") != 0 || Digits("USD") != 2 {
		t.Error("Digits does not match the formatter's own table")
	}
}
