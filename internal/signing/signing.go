// Package signing verifies the webhook signatures the payment processor sends.
//
// This is one half of a contract; the other half is the simulator's
// services/simulator/src/signing.ts. Both must agree on the exact bytes that
// get hashed: "<unix timestamp>.<raw body>". Change one side and every request
// fails with a signature mismatch that looks nothing like the real cause.
package signing

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// defaultTolerance is the window the tests verify against; the binaries take
// theirs from config.SignatureTolerance.
const defaultTolerance = 5 * time.Minute

var (
	// ErrMalformedHeader is a header without a parseable t= and v1= pair.
	ErrMalformedHeader = errors.New("signature header is malformed")
	// ErrStaleTimestamp is a header whose timestamp is outside the tolerance
	// window in either direction.
	ErrStaleTimestamp = errors.New("signature timestamp is outside the tolerance window")
	// ErrMismatch is a well-formed, fresh header whose HMAC does not match.
	ErrMismatch = errors.New("signature does not match")
)

// Header is the value of X-Processor-Signature: "t=1735689600,v1=<hex>".
type Header struct {
	Timestamp time.Time
	Signature []byte
}

// ParseHeader splits a header value into its timestamp and signature.
func ParseHeader(value string) (Header, error) {
	var (
		parsed Header
		gotTS  bool
		gotSig bool
	)

	for _, part := range strings.Split(value, ",") {
		key, val, found := strings.Cut(strings.TrimSpace(part), "=")
		if !found {
			continue
		}
		switch key {
		case "t":
			seconds, err := strconv.ParseInt(val, 10, 64)
			if err != nil {
				return Header{}, fmt.Errorf("%w: bad timestamp", ErrMalformedHeader)
			}
			parsed.Timestamp = time.Unix(seconds, 0).UTC()
			gotTS = true
		case "v1":
			raw, err := hex.DecodeString(val)
			if err != nil {
				return Header{}, fmt.Errorf("%w: signature is not hex", ErrMalformedHeader)
			}
			parsed.Signature = raw
			gotSig = true
		}
	}

	if !gotTS || !gotSig {
		return Header{}, fmt.Errorf("%w: want t= and v1=", ErrMalformedHeader)
	}
	return parsed, nil
}

// Compute returns the expected signature for a body at a point in time.
func Compute(secret string, body []byte, ts time.Time) []byte {
	mac := hmac.New(sha256.New, []byte(secret))
	// Writing the timestamp separately avoids allocating a joined buffer, and
	// hash.Hash.Write is documented never to return an error.
	mac.Write([]byte(strconv.FormatInt(ts.Unix(), 10)))
	mac.Write([]byte("."))
	mac.Write(body)
	return mac.Sum(nil)
}

// Verify checks a signature header against the body.
//
// The timestamp is checked first so an obviously stale request costs nothing,
// and the comparison is hmac.Equal rather than bytes.Equal: a byte-by-byte
// compare returns faster on a wrong first byte than on a wrong last one, which
// is enough to recover a valid signature one character at a time.
func Verify(secret string, body []byte, header string, now time.Time, tolerance time.Duration) error {
	parsed, err := ParseHeader(header)
	if err != nil {
		return err
	}

	drift := now.Sub(parsed.Timestamp)
	if drift < 0 {
		drift = -drift
	}
	if drift > tolerance {
		return fmt.Errorf("%w: off by %s", ErrStaleTimestamp, drift.Round(time.Second))
	}

	if !hmac.Equal(Compute(secret, body, parsed.Timestamp), parsed.Signature) {
		return ErrMismatch
	}
	return nil
}

// VerifyAny accepts a delivery signed with any one of the merchant's current
// secrets.
//
// This is what makes a signing key rotatable without downtime. Rotation is not
// an instant: the new key has to be published, senders have to pick it up, and
// requests signed with the old one keep arriving in the meantime. A single
// accepted secret forces a cutover where every in-flight delivery is rejected,
// which is why keys that can only be rotated with an outage never get rotated.
//
// Order matters for cost, not correctness: the newest secret is tried first, so
// the steady state is one hash rather than two.
func VerifyAny(secrets []string, body []byte, header string, now time.Time, tolerance time.Duration) error {
	if len(secrets) == 0 {
		return fmt.Errorf("%w: no secrets configured", ErrMismatch)
	}

	var lastErr error
	for _, secret := range secrets {
		lastErr = Verify(secret, body, header, now, tolerance)
		if lastErr == nil {
			return nil
		}
		// A malformed header or a stale timestamp fails identically for every
		// secret, so there is nothing to gain by hashing again.
		if errors.Is(lastErr, ErrMalformedHeader) || errors.Is(lastErr, ErrStaleTimestamp) {
			return lastErr
		}
	}
	return lastErr
}

// Sign produces a header in the same format, for tests and for any outbound
// webhooks this platform sends later.
func Sign(secret string, body []byte, ts time.Time) string {
	return fmt.Sprintf("t=%d,v1=%s", ts.Unix(), hex.EncodeToString(Compute(secret, body, ts)))
}
