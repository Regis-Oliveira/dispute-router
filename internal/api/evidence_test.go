package api

import "testing"

// safeName is the only thing standing between a caller-supplied filename and an
// S3 key, so it gets tested like it matters.
func TestSafeNameCannotEscapeItsPrefix(t *testing.T) {
	tests := map[string]string{
		"receipt.pdf":             "receipt.pdf",
		"../../../etc/passwd":     "passwd",
		"/absolute/path/scan.png": "scan.png",
		"..":                      "",
		"/":                       "",
		"":                        "",
		"   ":                     "",
		"delivery proof.pdf":      "delivery-proof.pdf",
		"invoice#42?v=2.pdf":      "invoice-42-v-2.pdf",
		"..\\windows\\system32":   "windows-system32",
		// path.Base takes everything after the last "/", so a shell-shaped
		// name loses its whole prefix before the character filter even runs.
		"nul;rm -rf /.txt":         "txt",
		"--leading-and-trailing--": "leading-and-trailing",
	}

	for input, want := range tests {
		if got := safeName(input); got != want {
			t.Errorf("safeName(%q) = %q, want %q", input, got, want)
		}
	}
}

func TestSafeNameIsBounded(t *testing.T) {
	long := ""
	for range 500 {
		long += "a"
	}
	if got := safeName(long); len(got) != 120 {
		t.Errorf("length = %d, want it capped at 120", len(got))
	}
}

// A whitelist, not a blacklist: evidence is a document or an image, and
// anything else is a mistake or somebody using the bucket as free hosting.
func TestOnlyDocumentTypesAreAccepted(t *testing.T) {
	for _, accepted := range []string{"application/pdf", "image/png", "image/jpeg", "text/plain", "text/csv"} {
		if _, ok := allowedTypes[accepted]; !ok {
			t.Errorf("%s should be accepted", accepted)
		}
	}
	for _, rejected := range []string{
		"text/html", "application/javascript", "image/svg+xml",
		"application/x-msdownload", "application/octet-stream", "",
	} {
		if _, ok := allowedTypes[rejected]; ok {
			t.Errorf("%s should not be accepted", rejected)
		}
	}
}
