// Package money is the one place an amount is turned into text.
//
// The rule everywhere in this system is that money is divided exactly once, at
// a formatter: the ledger stores minor units, the dashboard divides at the edge,
// the API never sends a float. This package is that formatter for every Go
// surface that hands a number to a reader - a prompt, an MCP tool result, a log
// line - so that "5799 USD" cannot be written by one caller while another
// writes "57.99 USD".
//
// It exists because of a letter that said $579.99 about a $57.99 dispute. The
// record handed a model amount_minor: 5799 with an instruction to copy amounts
// digit for digit, and it did. A model doing arithmetic on the way to a card
// network is a bug at the boundary, not in the model.
package money

import (
	"fmt"
	"strings"
)

// Not every currency has two decimal places, which is why this is a table and
// not the number 100. A blanket divide-by-100 turns 5,000 yen into 50 and
// nothing about the result looks wrong.
var minorDigits = map[string]int{"JPY": 0, "KRW": 0, "ISK": 0}

// Digits is how many minor-unit digits a currency has. Unknown currencies get
// two, which is wrong less often than any other guess.
func Digits(currency string) int {
	if d, ok := minorDigits[strings.ToUpper(currency)]; ok {
		return d
	}
	return 2
}

// FormatMinor renders an amount the way a letter should state it: "57.99 USD",
// "5,000 JPY". Code after the number, grouped thousands, no symbol - a form
// that is unambiguous to a reader and easy for a grader to find again.
func FormatMinor(amountMinor int64, currency string) string {
	digits := Digits(currency)
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
