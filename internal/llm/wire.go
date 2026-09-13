package llm

import (
	"context"
	"encoding/json"
)

// ContentBlock is one block in a message. The Messages API content array is
// polymorphic - text, tool_use, tool_result, thinking - and this is the union
// of the fields those shapes use, with omitempty keeping each one to the fields
// its own type actually carries.
type ContentBlock struct {
	Type string `json:"type"`

	// text
	Text string `json:"text,omitempty"`

	// tool_use
	ID    string          `json:"id,omitempty"`
	Name  string          `json:"name,omitempty"`
	Input json.RawMessage `json:"input,omitempty"`

	// tool_result
	ToolUseID string `json:"tool_use_id,omitempty"`
	Content   string `json:"content,omitempty"`
	IsError   bool   `json:"is_error,omitempty"`

	// thinking and redacted_thinking. A model may reason before it calls a
	// tool, and the API requires that reasoning to come back verbatim, with
	// its signature, on the next turn. The first multi-turn run against a real
	// model failed with "thinking.thinking: Field required": the loop had
	// echoed the block back with its text dropped, because these fields did
	// not exist. Single-turn flows never notice; a loop has to round-trip
	// everything it is handed.
	Thinking  string `json:"thinking,omitempty"`
	Signature string `json:"signature,omitempty"`
	Data      string `json:"data,omitempty"`

	// raw is the block exactly as the API sent it, and it is what goes back.
	// Naming the fields above is not enough: a block the API returns can carry
	// a field this struct does not know, or an empty string that omitempty
	// would drop, and either way the API refuses the echo. A block we build
	// ourselves has no raw form and marshals from its fields.
	//
	// It is unexported, which is what makes the pair of methods below the only
	// way in or out: a block round-trips verbatim or it does not round-trip,
	// and no caller in another package can put a raw form on a block it made up.
	raw json.RawMessage
}

// contentBlockFields is ContentBlock without its methods, so the default
// encoding can be reached from inside the custom one.
type contentBlockFields ContentBlock

// UnmarshalJSON decodes the fields and keeps the raw block for the echo.
//
// The pointer receiver is load-bearing: only a pointer method can store the raw
// bytes, and encoding/json only reaches it when it is decoding into an
// addressable value - which a []ContentBlock element is.
func (b *ContentBlock) UnmarshalJSON(data []byte) error {
	var fields contentBlockFields
	if err := json.Unmarshal(data, &fields); err != nil {
		return err
	}
	*b = ContentBlock(fields)
	b.raw = append(json.RawMessage(nil), data...)
	return nil
}

// MarshalJSON sends a block the API produced back verbatim and encodes one
// this package built from its fields.
//
// The value receiver is load-bearing too, and asymmetric with UnmarshalJSON on
// purpose: blocks travel as []ContentBlock inside a Message, and a marshaller
// declared on the pointer would not be found for a slice of values - every
// echoed block would silently lose its raw form and the API would refuse it.
func (b ContentBlock) MarshalJSON() ([]byte, error) {
	if len(b.raw) > 0 {
		return b.raw, nil
	}
	return json.Marshal(contentBlockFields(b))
}

// Message is one turn of the conversation, from either side.
type Message struct {
	Role    string         `json:"role"`
	Content []ContentBlock `json:"content"`
}

// Request is one call to the Messages API.
type Request struct {
	Model     string    `json:"model,omitempty"`
	System    string    `json:"system,omitempty"`
	Messages  []Message `json:"messages"`
	Tools     []Tool    `json:"tools,omitempty"`
	MaxTokens int       `json:"max_tokens"`

	// Temperature is a pointer so that "unset" is distinguishable from 0,
	// which is a temperature someone may well want.
	Temperature *float64 `json:"temperature,omitempty"`

	ToolChoice *ToolChoice `json:"tool_choice,omitempty"`

	// CacheSystem asks the provider to cache everything up to and including the
	// system prompt - which means the tool schemas too, since they come first
	// in the cacheable prefix.
	//
	// Worth doing here because the system prompts are the largest constant in
	// the request: about 1,570 tokens across the two calls, against roughly 880
	// for the record that actually varies. Nearly half of every input is text
	// that has not changed since the process started.
	//
	// It is a request, not a guarantee. Providers impose a minimum cacheable
	// length and these prompts sit close to it, so whether it takes is a
	// question for the usage numbers rather than for this comment.
	CacheSystem bool `json:"-"`
}

// ToolChoice constrains what the model may do with the tools it was given.
// "auto" lets it decide, "any" forces some tool, and "tool" with a Name forces
// one particular tool - which is how a caller gets a structured answer instead
// of prose it has to parse.
type ToolChoice struct {
	Type string `json:"type"`
	Name string `json:"name,omitempty"`
}

// Tool is one entry in the request's "tools" array.
type Tool struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"input_schema"`
}

// ToolUse is a tool_use content block from the model, in the form a dispatcher
// wants it: the id the answer has to quote, the name, and the arguments.
type ToolUse struct {
	ID    string          `json:"id"`
	Name  string          `json:"name"`
	Input json.RawMessage `json:"input"`
}

// Usage is the token count of one response, or of several added together.
type Usage struct {
	InputTokens              int `json:"input_tokens"`
	OutputTokens             int `json:"output_tokens"`
	CacheCreationInputTokens int `json:"cache_creation_input_tokens,omitempty"`
	CacheReadInputTokens     int `json:"cache_read_input_tokens,omitempty"`
}

// Add accumulates one response's usage into a running total.
func (u *Usage) Add(other Usage) {
	u.InputTokens += other.InputTokens
	u.OutputTokens += other.OutputTokens
	u.CacheCreationInputTokens += other.CacheCreationInputTokens
	u.CacheReadInputTokens += other.CacheReadInputTokens
}

// Response is what the Messages API answered.
type Response struct {
	Content    []ContentBlock `json:"content"`
	StopReason string         `json:"stop_reason"`
	Usage      Usage          `json:"usage"`
}

// Completer is the seam between a caller and whatever answers it.
//
// What sits above this interface is the part being built and the part worth
// testing; the transport is not. Behind it sits Anthropic in production and a
// scripted double in the tests, which is also what lets the eval set run
// deterministically and for free. The same shape as the Publisher seam in
// internal/outbox, for the same reason: when SQS replaced the log publisher,
// the relay did not change a line.
type Completer interface {
	Complete(ctx context.Context, req Request) (Response, error)
}
