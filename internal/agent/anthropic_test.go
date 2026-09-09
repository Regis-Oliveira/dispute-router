package agent

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// against stands a fake API in front of the client, so the wire format can be
// checked without spending anything. Every real call to this endpoint costs
// money, which makes free coverage of the part most likely to be wrong - a
// header name, a field name - worth having.
func against(t *testing.T, handler http.HandlerFunc) *Anthropic {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)

	client, err := NewAnthropic("sk-test-key", "claude-test")
	if err != nil {
		t.Fatalf("NewAnthropic: %v", err)
	}
	client.endpoint = server.URL
	client.client = &http.Client{Timeout: 5 * time.Second}
	return client
}

func TestTheRequestCarriesWhatTheAPIExpects(t *testing.T) {
	var seen struct {
		key, version, contentType string
		body                      anthropicBody
	}

	client := against(t, func(w http.ResponseWriter, r *http.Request) {
		seen.key = r.Header.Get("x-api-key")
		seen.version = r.Header.Get("anthropic-version")
		seen.contentType = r.Header.Get("content-type")
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &seen.body)

		// Deliberately checked here rather than asserted on a struct: the model
		// belongs in the body for this API and in the call for Bedrock, and
		// getting that backwards is the mistake this test exists for.
		if !strings.Contains(string(raw), `"model"`) {
			t.Error("the request body carries no model")
		}
		if strings.Contains(string(raw), "anthropic_version") {
			t.Error("the request body carries anthropic_version, which is Bedrock's field")
		}

		w.Header().Set("content-type", "application/json")
		_, _ = w.Write([]byte(`{"content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn","usage":{"input_tokens":10,"output_tokens":2}}`))
	})

	response, err := client.Complete(context.Background(), Request{
		System:    "be careful",
		MaxTokens: 1024,
		Messages:  []Message{{Role: "user", Content: []ContentBlock{{Type: "text", Text: "hi"}}}},
	})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}

	if seen.key != "sk-test-key" {
		t.Errorf("x-api-key = %q", seen.key)
	}
	if seen.version != anthropicVersion {
		t.Errorf("anthropic-version = %q, want %q", seen.version, anthropicVersion)
	}
	if seen.contentType != "application/json" {
		t.Errorf("content-type = %q", seen.contentType)
	}
	if seen.body.Model != "claude-test" || seen.body.MaxTokens != 1024 || seen.body.System != "be careful" {
		t.Errorf("body = %+v", seen.body)
	}
	if response.StopReason != "end_turn" || response.Usage.InputTokens != 10 {
		t.Errorf("response = %+v", response)
	}
}

// Overload is transient and common. Failing a whole eval case on one wastes
// the tokens already spent getting there.
func TestOverloadIsRetried(t *testing.T) {
	var calls atomic.Int32

	client := against(t, func(w http.ResponseWriter, _ *http.Request) {
		if calls.Add(1) == 1 {
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte(`{"error":{"message":"rate limited"}}`))
			return
		}
		_, _ = w.Write([]byte(`{"content":[],"stop_reason":"end_turn","usage":{}}`))
	})

	if _, err := client.Complete(context.Background(), Request{MaxTokens: 16}); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if got := calls.Load(); got != 2 {
		t.Errorf("%d calls; a 429 was not retried exactly once before succeeding", got)
	}
}

// A bad request is not transient, and retrying it three times bills three
// times for the same mistake.
func TestABadRequestIsNotRetried(t *testing.T) {
	var calls atomic.Int32

	client := against(t, func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":{"message":"max_tokens is required"}}`))
	})

	_, err := client.Complete(context.Background(), Request{})
	if err == nil {
		t.Fatal("a 400 was reported as success")
	}
	if calls.Load() != 1 {
		t.Errorf("%d calls; a permanent error was retried", calls.Load())
	}
	// The API's own message has to survive. "400" alone sends somebody to the
	// wrong place; the sentence after it does not.
	if !strings.Contains(err.Error(), "max_tokens is required") {
		t.Errorf("the API's message was discarded: %v", err)
	}
}

func TestAResponseWithoutAStopReasonIsAnError(t *testing.T) {
	client := against(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"content":[],"usage":{}}`))
	})
	if _, err := client.Complete(context.Background(), Request{MaxTokens: 16}); err == nil {
		t.Fatal("a response with no stop_reason was accepted; callers branch on it")
	}
}

// The message names the confusion it exists to prevent.
func TestAMissingKeyIsRefusedWithTheReason(t *testing.T) {
	_, err := NewAnthropic("", "claude-test")
	if err == nil {
		t.Fatal("an empty key was accepted")
	}
	if !strings.Contains(err.Error(), "claude.ai") {
		t.Errorf("the error does not mention that a subscription is a different product: %v", err)
	}
}
