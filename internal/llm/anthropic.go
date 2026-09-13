package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// Anthropic is the Completer that talks to the Messages API directly.
//
// It is written against net/http rather than the official SDK on purpose, and
// the reason is specific to this codebase rather than general: the wire types
// in loop.go are kept SDK-free so another completer can share them. Adding the
// SDK would mean maintaining a second representation of the same request and
// converting between them at this boundary, in a project whose point is
// understanding the mechanics. In a greenfield application the SDK would be
// the default and this would be the wrong call.
//
// Note for anyone reading this expecting it to work with a claude.ai
// subscription: it will not. The consumer product and the API are separate,
// with separate billing, and there is no supported way for a service to call
// the former. This needs a key from console.anthropic.com.
type Anthropic struct {
	client *http.Client
	key    string
	model  string
	// endpoint is overridable so the wire format can be tested against a local
	// server. What this file gets wrong is a header or a field name, and that
	// is exactly what a test can catch for nothing.
	endpoint string
}

const (
	anthropicEndpoint = "https://api.anthropic.com/v1/messages"
	anthropicVersion  = "2023-06-01"
)

// NewAnthropic builds the client.
//
// The key arrives as a value read from the environment by the caller, never
// from a command-line flag: a flag lands in shell history and in the output of
// ps, where every other process on the machine can read it.
func NewAnthropic(key, model string) (*Anthropic, error) {
	if key == "" {
		return nil, errors.New("agent: ANTHROPIC_API_KEY is required (a key from console.anthropic.com; " +
			"a claude.ai subscription is a different product and cannot be used here)")
	}
	if model == "" {
		return nil, errors.New("agent: ANTHROPIC_MODEL is required")
	}
	return &Anthropic{
		// A timeout, because the default http.Client has none and a hung
		// request would sit there until the process is killed - holding a
		// dispute in 'resolving' the whole time.
		client:   &http.Client{Timeout: 2 * time.Minute},
		key:      key,
		model:    model,
		endpoint: anthropicEndpoint,
	}, nil
}

// cacheControl marks the end of a cacheable prefix. Ephemeral is the only kind
// there is; the name refers to a short time to live, not to whether it works.
type cacheControl struct {
	Type string `json:"type"`
}

type systemBlock struct {
	Type         string        `json:"type"`
	Text         string        `json:"text"`
	CacheControl *cacheControl `json:"cache_control,omitempty"`
}

// anthropicBody is Request as the API reads it: the model named in the body,
// the version in a header, and the system prompt in whichever of its two
// shapes the cache breakpoint needs.
type anthropicBody struct {
	Model     string `json:"model"`
	MaxTokens int    `json:"max_tokens"`
	// A string when nothing is cached, an array of blocks when something is:
	// cache_control lives on a block and a bare string has nowhere to put it.
	System      any         `json:"system,omitempty"`
	Messages    []Message   `json:"messages"`
	Tools       []Tool      `json:"tools,omitempty"`
	ToolChoice  *ToolChoice `json:"tool_choice,omitempty"`
	Temperature *float64    `json:"temperature,omitempty"`
}

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

// Complete sends one request, retrying a busy API up to maxAttempts times.
func (a *Anthropic) Complete(ctx context.Context, req Request) (Response, error) {
	body, err := json.Marshal(anthropicBody{
		Model:       a.model,
		MaxTokens:   req.MaxTokens,
		System:      systemFor(req),
		Messages:    req.Messages,
		Tools:       req.Tools,
		ToolChoice:  req.ToolChoice,
		Temperature: req.Temperature,
	})
	if err != nil {
		return Response{}, fmt.Errorf("anthropic: encoding request: %w", err)
	}

	var lastErr error
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		response, err := a.once(ctx, body)
		if err == nil {
			return response, nil
		}
		lastErr = err
		if !retryable(err) || attempt == maxAttempts {
			break
		}
		// Linear backoff: two seconds, then four. Not exponential, because
		// three attempts never get far enough apart for the difference to
		// matter, and a predictable delay is easier to reason about when
		// something is being billed for. Where the API says how long to wait,
		// its answer wins over this arithmetic.
		select {
		case <-ctx.Done():
			return Response{}, ctx.Err()
		case <-time.After(delayFor(err, time.Duration(attempt)*2*time.Second)):
		}
	}
	return Response{}, lastErr
}

func (a *Anthropic) once(ctx context.Context, body []byte) (Response, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, a.endpoint, bytes.NewReader(body))
	if err != nil {
		return Response{}, fmt.Errorf("anthropic: building request: %w", err)
	}
	request.Header.Set("content-type", "application/json")
	request.Header.Set("anthropic-version", anthropicVersion)
	request.Header.Set("x-api-key", a.key)

	res, err := a.client.Do(request)
	if err != nil {
		// A transport failure is worth one more try; a cancelled context is
		// not, and is returned as a plain error so nothing retries it.
		if ctx.Err() != nil {
			return Response{}, fmt.Errorf("anthropic: %w", err)
		}
		return Response{}, &apiError{op: "anthropic", err: err}
	}
	defer res.Body.Close()

	// Bounded: an error body from a proxy can be a whole HTML page, and it ends
	// up in a log and in an agent_runs row.
	payload, err := io.ReadAll(io.LimitReader(res.Body, 1<<20))
	if err != nil {
		return Response{}, &apiError{op: "anthropic", err: fmt.Errorf("reading response: %w", err)}
	}

	if res.StatusCode != http.StatusOK {
		return Response{}, &apiError{
			op:         "anthropic",
			statusCode: res.StatusCode,
			status:     res.Status,
			body:       string(bytes.TrimSpace(payload)),
			retryAfter: parseRetryAfter(res.Header),
		}
	}

	var response Response
	if err := json.Unmarshal(payload, &response); err != nil {
		return Response{}, fmt.Errorf("anthropic: decoding response: %w", err)
	}
	if response.StopReason == "" {
		return Response{}, errors.New("anthropic: response carried no stop_reason")
	}
	return response, nil
}

// systemFor renders the system prompt, with a cache breakpoint when one was
// asked for.
//
// The breakpoint goes on the system block rather than on a tool, because the
// cacheable prefix runs tools then system then messages: marking the system
// caches the tool schemas with it, and marking a tool would cache only the
// tools. One breakpoint, the larger prefix.
func systemFor(req Request) any {
	if req.System == "" {
		return nil
	}
	if !req.CacheSystem {
		return req.System
	}
	return []systemBlock{{
		Type:         "text",
		Text:         req.System,
		CacheControl: &cacheControl{Type: "ephemeral"},
	}}
}
