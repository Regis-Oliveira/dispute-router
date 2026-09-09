package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"
)

// Anthropic is the Completer that talks to the API directly, for anyone with an
// API key and no AWS account.
//
// It is written against net/http rather than the official SDK on purpose, and
// the reason is specific to this codebase rather than general: the wire types
// in loop.go already exist because Bedrock's InvokeModel takes that JSON
// verbatim. Adding the SDK would mean maintaining a second representation of
// the same request and converting between them at this boundary, in a project
// whose point is understanding the mechanics. In a greenfield application the
// SDK would be the default and this would be the wrong call.
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

// anthropicBody differs from Bedrock's in exactly one way: the model is named
// in the body rather than in the call, and there is no anthropic_version field.
type anthropicBody struct {
	Model       string      `json:"model"`
	MaxTokens   int         `json:"max_tokens"`
	System      string      `json:"system,omitempty"`
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

func (a *Anthropic) Complete(ctx context.Context, req Request) (Response, error) {
	body, err := json.Marshal(anthropicBody{
		Model:       a.model,
		MaxTokens:   req.MaxTokens,
		System:      req.System,
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
		response, retryable, err := a.once(ctx, body)
		if err == nil {
			return response, nil
		}
		lastErr = err
		if !retryable || attempt == maxAttempts {
			break
		}
		// Fixed backoff rather than exponential: three attempts never get far
		// enough apart for the difference to matter, and a predictable delay is
		// easier to reason about when something is being billed for.
		select {
		case <-ctx.Done():
			return Response{}, ctx.Err()
		case <-time.After(time.Duration(attempt) * 2 * time.Second):
		}
	}
	return Response{}, lastErr
}

func (a *Anthropic) once(ctx context.Context, body []byte) (Response, bool, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, a.endpoint, bytes.NewReader(body))
	if err != nil {
		return Response{}, false, fmt.Errorf("anthropic: building request: %w", err)
	}
	request.Header.Set("content-type", "application/json")
	request.Header.Set("anthropic-version", anthropicVersion)
	request.Header.Set("x-api-key", a.key)

	res, err := a.client.Do(request)
	if err != nil {
		// A transport failure is worth one more try; a cancelled context is not.
		return Response{}, ctx.Err() == nil, fmt.Errorf("anthropic: %w", err)
	}
	defer res.Body.Close()

	// Bounded: an error body from a proxy can be a whole HTML page, and it ends
	// up in a log and in an agent_runs row.
	payload, err := io.ReadAll(io.LimitReader(res.Body, 1<<20))
	if err != nil {
		return Response{}, true, fmt.Errorf("anthropic: reading response: %w", err)
	}

	if res.StatusCode != http.StatusOK {
		// The API's own message is included. "429" alone sends somebody to the
		// wrong place; "429: this key has no credit" does not.
		retryable := res.StatusCode == http.StatusTooManyRequests ||
			res.StatusCode == 529 ||
			res.StatusCode >= 500
		return Response{}, retryable, fmt.Errorf("anthropic: %s: %s", res.Status, bytes.TrimSpace(payload))
	}

	var response Response
	if err := json.Unmarshal(payload, &response); err != nil {
		return Response{}, false, fmt.Errorf("anthropic: decoding response: %w", err)
	}
	if response.StopReason == "" {
		return Response{}, false, errors.New("anthropic: response carried no stop_reason")
	}
	return response, false, nil
}
