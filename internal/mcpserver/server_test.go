package mcpserver

import "testing"

// The mask has to keep "is this the same person" answerable while giving away
// nothing about who that person is.
func TestMaskEmail(t *testing.T) {
	tests := map[string]string{
		"zeca.zanetti17283@inbox.test": "z***@inbox.test",
		"a@b.com":                      "a@b.com",
		"ab@example.com":               "a***@example.com",
		"":                             "***",
		"no-at-sign":                   "***",
		"@leading.com":                 "***",
	}
	for input, want := range tests {
		if got := maskEmail(input); got != want {
			t.Errorf("maskEmail(%q) = %q, want %q", input, got, want)
		}
	}
}

// A masked address must never contain the original local part - that is the
// whole job.
func TestMaskEmailNeverLeaksTheLocalPart(t *testing.T) {
	for _, address := range []string{
		"charlotte.henriques@example.com",
		"finance-team@merchant.test",
		"a.very.long.address.indeed@mail.test",
	} {
		masked := maskEmail(address)
		local := address[:len(address)-len("@example.com")]
		if len(local) > 1 && contains(masked, local) {
			t.Errorf("maskEmail(%q) = %q, which still contains the local part", address, masked)
		}
	}
}

func contains(haystack, needle string) bool {
	return len(needle) > 0 && len(haystack) >= len(needle) &&
		indexOf(haystack, needle) >= 0
}

func indexOf(haystack, needle string) int {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return i
		}
	}
	return -1
}

func TestSplitCSV(t *testing.T) {
	tests := map[string][]string{
		"":                     nil,
		"   ":                  nil,
		"alert":                {"alert"},
		"alert,chargeback":     {"alert", "chargeback"},
		" alert , chargeback ": {"alert", "chargeback"},
		"alert,,chargeback":    {"alert", "chargeback"},
		",":                    {},
	}
	for input, want := range tests {
		got := splitCSV(input)
		if len(got) != len(want) {
			t.Errorf("splitCSV(%q) = %v, want %v", input, got, want)
			continue
		}
		for i := range got {
			if got[i] != want[i] {
				t.Errorf("splitCSV(%q) = %v, want %v", input, got, want)
				break
			}
		}
	}
}

// The cap is a context limit, not a performance one, so it has to be small
// enough that a full page still leaves room to reason about the results.
func TestRowCapIsSane(t *testing.T) {
	if maxRows <= 0 || maxRows > 100 {
		t.Errorf("maxRows = %d; a list tool that can return more than ~100 rows fills the context it was meant to inform", maxRows)
	}
}
