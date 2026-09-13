package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"

	"github.com/google/jsonschema-go/jsonschema"

	"github.com/regisoliveira/dispute-router/internal/llm"
)

// toolCall is one forced single-turn call: a system prompt, exactly one tool,
// and no way for the model to answer any other way.
//
// The generator and the verifier are the same call with different words in it,
// and they used to be the same seventy lines written twice. What actually
// differs between them is the prompt, the tool, and what a parsed answer means
// - so those are fields here, and the meaning stays with the caller.
type toolCall struct {
	completer llm.Completer
	model     string
	pricing   llm.Pricing
	maxTokens int

	// label prefixes every error this call produces, and noun names what was
	// being read when one could not be parsed. A caller reading "verifier:
	// unreadable verdict" knows which of the two calls failed without a stack.
	label string
	noun  string

	system      string
	tool        string
	description string
	schema      func() json.RawMessage
}

// billed is what a call cost. It is returned separately from what the call
// produced, and on every path including the failures: the tokens were spent
// whether or not an answer came back, and a run that reports zero spend for a
// failed call is how a budget quietly stops meaning anything.
type billed struct {
	usage      llm.Usage
	costMicros int64
}

// callTool makes the call and decodes the tool input into T.
//
// It decides nothing about the answer. Whether a recommendation is one of the
// two the host understands, whether a verdict with findings may still call
// itself a pass - those are the callers', because they are the rules that keep
// this package fail-closed and they do not belong in a transport helper.
func callTool[T any](ctx context.Context, call toolCall, prompt string) (T, billed, error) {
	var zero T

	response, err := call.completer.Complete(ctx, llm.Request{
		Model:     call.model,
		System:    call.system,
		MaxTokens: call.maxTokens,
		Messages: []llm.Message{{
			Role:    "user",
			Content: []llm.ContentBlock{{Type: "text", Text: prompt}},
		}},
		Tools: []llm.Tool{{
			Name:        call.tool,
			Description: call.description,
			InputSchema: call.schema(),
		}},
		// Forced, so the answer arrives as a structure rather than as prose
		// that has to be interpreted - and interpreting prose is where a
		// "no problems found" becomes a pass by accident.
		ToolChoice: &llm.ToolChoice{Type: "tool", Name: call.tool},
		// The system prompt and the tool schema are identical on every call;
		// only the record below them changes.
		CacheSystem: true,
	})
	if err != nil {
		return zero, billed{}, fmt.Errorf("%s: %w", call.label, err)
	}

	paid := billed{usage: response.Usage, costMicros: call.pricing.Cost(response.Usage)}

	if response.StopReason == "max_tokens" {
		// An answer cut off partway is not a short answer. A letter stops
		// mid-argument and a verdict stops mid-way through listing its
		// findings, and either one read as finished is the failure this check
		// exists for.
		return zero, paid, fmt.Errorf("%s: response was truncated at max_tokens", call.label)
	}

	for _, block := range response.Content {
		if block.Type != "tool_use" || block.Name != call.tool {
			continue
		}
		var parsed T
		if err := json.Unmarshal(block.Input, &parsed); err != nil {
			return zero, paid, fmt.Errorf("%s: unreadable %s: %w", call.label, call.noun, err)
		}
		return parsed, paid, nil
	}

	return zero, paid, fmt.Errorf("%s: no %s call in the response (stop_reason %q)",
		call.label, call.tool, response.StopReason)
}

// draftSchema and verdictSchema are the two tool schemas, derived once.
//
// They were derived and marshalled on every call before, which bought nothing:
// the structs are fixed at compile time, so the bytes are the same on the
// thousandth call as on the first. Deriving them once also makes them a
// program-level fact, which is what lets promptFingerprint hash the same bytes
// the model was actually sent.
var (
	draftSchema   = sync.OnceValue(mustSchema[draftInput])
	verdictSchema = sync.OnceValue(mustSchema[verdictInput])
)

// mustSchema derives one struct's JSON schema, or panics.
//
// The panic is the point, and it is what the old mustSchema did not do despite
// the name: it returned a schema whose entire content was the error message,
// which then went to the model as the tool's description and to the prompt
// fingerprint as a stable-looking hash. A struct this package cannot describe
// is a program that cannot work, not a request that went wrong, and it fails
// the first time a schema is asked for rather than on every call after.
func mustSchema[T any]() json.RawMessage {
	schema, err := jsonschema.For[T](nil)
	if err != nil {
		panic(fmt.Sprintf("agent: deriving the JSON schema for %T: %v", *new(T), err))
	}
	encoded, err := json.Marshal(schema)
	if err != nil {
		panic(fmt.Sprintf("agent: encoding the JSON schema for %T: %v", *new(T), err))
	}
	return encoded
}
