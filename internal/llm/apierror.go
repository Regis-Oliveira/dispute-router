package llm

import (
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// maxAttempts bounds the retries on a busy API.
//
// Three, with a short backoff. Overload is transient and common enough that
// failing a whole eval case on one is wasteful; retrying forever is how a
// cost ceiling stops meaning anything, which is why this is a small number and
// not a loop.
const maxAttempts = 3

// maxRetryAfter caps what a provider may ask us to wait.
//
// Retry-After is a value from outside the process, and an unbounded one would
// hold a dispute in 'resolving' for as long as the header says. A minute is
// longer than any real rate-limit window this system meets and still short
// enough that a stuck run is noticed rather than waited on.
const maxRetryAfter = time.Minute

// apiError is a model provider's answer, or its failure to answer one.
//
// One type for both providers, because there was one question - is this worth
// another attempt - answered two different ways: Anthropic returned a bool
// alongside the error, which nothing could inspect after the fact, and Voyage
// wrapped a bare errRateLimited sentinel, which said a rate limit had happened
// and not what the API had actually returned. Neither exposed the status, so
// neither could tell a caller whether the key was out of credit or the API was
// busy.
type apiError struct {
	// op names the provider, for the message: the two clients produce the same
	// type and an error that does not say which one it came from is worse than
	// the string it replaced.
	op string
	// statusCode is zero when the request never reached an answer, in which
	// case err carries the transport failure.
	statusCode int
	status     string
	body       string
	// retryAfter is what the response asked for, zero when it asked for
	// nothing.
	retryAfter time.Duration
	err        error
}

func (e *apiError) Error() string {
	if e.statusCode == 0 {
		return fmt.Sprintf("%s: %v", e.op, e.err)
	}
	// The API's own message is included. "429" alone sends somebody to the
	// wrong place; "429: this key has no credit" does not.
	return fmt.Sprintf("%s: %s: %s", e.op, e.status, e.body)
}

func (e *apiError) Unwrap() error { return e.err }

// Retryable reports whether another attempt is worth paying for.
//
// A request that never got an answer is worth one more; so is a rate limit, an
// overload (529 is Anthropic's) and anything else the server blames on itself.
// Everything else - a bad key, a malformed request - fails the same way three
// times and bills for the privilege.
func (e *apiError) Retryable() bool {
	if e.statusCode == 0 {
		return e.err != nil
	}
	return e.statusCode == http.StatusTooManyRequests ||
		e.statusCode == 529 ||
		e.statusCode >= 500
}

// retryable is the question both retry loops ask.
func retryable(err error) bool {
	var apiErr *apiError
	return errors.As(err, &apiErr) && apiErr.Retryable()
}

// delayFor is how long to wait before the next attempt: what the provider
// asked for where it said, and the caller's own computed delay where it did
// not.
func delayFor(err error, computed time.Duration) time.Duration {
	var apiErr *apiError
	if errors.As(err, &apiErr) && apiErr.retryAfter > 0 {
		return apiErr.retryAfter
	}
	return computed
}

// parseRetryAfter reads the header in both of its forms, seconds and an HTTP
// date, and reports zero for anything it cannot use - including a date already
// in the past, which asks for no wait at all.
func parseRetryAfter(header http.Header) time.Duration {
	value := strings.TrimSpace(header.Get("Retry-After"))
	if value == "" {
		return 0
	}
	if seconds, err := strconv.Atoi(value); err == nil {
		if seconds <= 0 {
			return 0
		}
		return min(time.Duration(seconds)*time.Second, maxRetryAfter)
	}
	if when, err := http.ParseTime(value); err == nil {
		if wait := time.Until(when); wait > 0 {
			return min(wait, maxRetryAfter)
		}
	}
	return 0
}
