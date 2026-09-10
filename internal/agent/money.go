package agent

import (
	"fmt"
	"strings"
)

// Money crosses into a prompt formatted, never as raw minor units.
//
// This file exists because of a letter that said $579.99 about a $57.99
// dispute. The record handed the model amount_minor: 5799 and the prompt told
// it to copy amounts from the record digit for digit - which is exactly what it
// did. Another draft copied 2790 straight through for a charge of $27.90.
//
// The rule everywhere else in this system is that money is divided exactly
// once, at the formatter: the ledger stores minor units, the dashboard divides
// at the edge, and the API never sends a float. The prompt was the one boundary
// that got an undivided integer and a human-language instruction to transcribe
// it. A model doing arithmetic on the way to a card network is a bug in the
// prompt, not in the model.
//
// Not every currency has two decimal places, which is why this is a table and
// not the number 100.
var minorDigits = map[string]int{"JPY": 0, "KRW": 0, "ISK": 0}

func digitsFor(currency string) int {
	if d, ok := minorDigits[strings.ToUpper(currency)]; ok {
		return d
	}
	return 2
}

// FormatMinor renders an amount the way a letter should state it.
func FormatMinor(amountMinor int64, currency string) string {
	digits := digitsFor(currency)
	if digits == 0 {
		return fmt.Sprintf("%s %s", group(amountMinor), strings.ToUpper(currency))
	}

	scale := int64(1)
	for range digits {
		scale *= 10
	}
	whole, frac := amountMinor/scale, amountMinor%scale
	if frac < 0 {
		frac = -frac
	}
	return fmt.Sprintf("%s.%0*d %s", group(whole), digits, frac, strings.ToUpper(currency))
}

// group inserts thousands separators. A four-figure amount written without one
// is the shape a misplaced decimal takes, and the letters this produces are
// read by somebody deciding whether to reverse a charge.
func group(n int64) string {
	sign := ""
	if n < 0 {
		sign, n = "-", -n
	}
	digits := fmt.Sprintf("%d", n)

	var b strings.Builder
	for i, r := range digits {
		if i > 0 && (len(digits)-i)%3 == 0 {
			b.WriteByte(',')
		}
		b.WriteRune(r)
	}
	return sign + b.String()
}
