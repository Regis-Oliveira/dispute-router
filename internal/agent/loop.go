package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"golang.org/x/sync/errgroup"
)

// ---------------------------------------------------------------------------
// wire types
// ---------------------------------------------------------------------------

// ContentBlock is one block in a message. The Messages API content array is
// polymorphic - text, tool_use, tool_result, thinking - and this is the union
// of the fields those shapes use, with omitempty keeping each one to the fields
// its own type actually carries.
//
// Written out rather than imported, for the reason given at the top of
// tools.go: Bedrock's InvokeModel takes this JSON verbatim.
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
	raw json.RawMessage
}

// contentBlockFields is ContentBlock without its methods, so the default
// encoding can be reached from inside the custom one.
type contentBlockFields ContentBlock

func (b *ContentBlock) UnmarshalJSON(data []byte) error {
	var fields contentBlockFields
	if err := json.Unmarshal(data, &fields); err != nil {
		return err
	}
	*b = ContentBlock(fields)
	b.raw = append(json.RawMessage(nil), data...)
	return nil
}

func (b ContentBlock) MarshalJSON() ([]byte, error) {
	if len(b.raw) > 0 {
		return b.raw, nil
	}
	return json.Marshal(contentBlockFields(b))
}

type Message struct {
	Role    string         `json:"role"`
	Content []ContentBlock `json:"content"`
}

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

type Usage struct {
	InputTokens              int `json:"input_tokens"`
	OutputTokens             int `json:"output_tokens"`
	CacheCreationInputTokens int `json:"cache_creation_input_tokens,omitempty"`
	CacheReadInputTokens     int `json:"cache_read_input_tokens,omitempty"`
}

// Add accumulates one response's usage into a running total. Exported because
// the eval package totals a run, and a token count that cannot leave this
// package cannot explain a bill.
func (u *Usage) Add(other Usage) {
	u.InputTokens += other.InputTokens
	u.OutputTokens += other.OutputTokens
	u.CacheCreationInputTokens += other.CacheCreationInputTokens
	u.CacheReadInputTokens += other.CacheReadInputTokens
}

type Response struct {
	Content    []ContentBlock `json:"content"`
	StopReason string         `json:"stop_reason"`
	Usage      Usage          `json:"usage"`
}

// Completer is the seam between the loop and whatever answers it.
//
// The loop is the part being built and the part worth testing; the transport is
// not. Behind this interface sits Bedrock in production and a scripted fake in
// the tests, which is also what lets the eval set run deterministically and for
// free. The same shape as the Publisher seam in internal/outbox, for the same
// reason: when SQS replaced the log publisher, the relay did not change a line.
type Completer interface {
	Complete(ctx context.Context, req Request) (Response, error)
}

// ---------------------------------------------------------------------------
// money
// ---------------------------------------------------------------------------

// Cost is carried in micro-dollars as an int64, for the same reason the ledger
// carries money in minor units: a budget compared with floating point drifts,
// and a ceiling that drifts is not a ceiling. Cents are too coarse here - a run
// can legitimately cost a third of one - so the unit is 1e-6 USD.
type Pricing struct {
	InputMicrosPerMTok  int64
	OutputMicrosPerMTok int64

	// Cached tokens bill at their own rates. Left at zero they are DERIVED from
	// the input rate rather than set equal to it.
	//
	// Falling back to the input rate was the first version and it was safe for a
	// ceiling and useless for a measurement: a cache that worked perfectly would
	// report no saving at all, because every read was billed as a fresh token.
	// The multipliers below are the standard ones - a write costs a quarter more
	// than fresh input, a read a tenth of it - and they are worth checking
	// against current pricing, because every number this system reports about
	// caching is only as right as they are.
	CacheWriteMicrosPerMTok int64
	CacheReadMicrosPerMTok  int64
}

const (
	cacheWriteMultiplier = 1.25
	cacheReadMultiplier  = 0.10
)

func (p Pricing) cost(u Usage) int64 {
	rate := func(configured int64, multiplier float64) int64 {
		if configured > 0 {
			return configured
		}
		return int64(float64(p.InputMicrosPerMTok) * multiplier)
	}
	const perMTok = 1_000_000
	return int64(u.InputTokens)*p.InputMicrosPerMTok/perMTok +
		int64(u.OutputTokens)*p.OutputMicrosPerMTok/perMTok +
		int64(u.CacheCreationInputTokens)*rate(p.CacheWriteMicrosPerMTok, cacheWriteMultiplier)/perMTok +
		int64(u.CacheReadInputTokens)*rate(p.CacheReadMicrosPerMTok, cacheReadMultiplier)/perMTok
}

// ---------------------------------------------------------------------------
// halting
// ---------------------------------------------------------------------------

// Halt says why the loop stopped, and the distinction it draws is the whole
// reason it exists: HaltCompleted means the model finished, and every other
// value means it was cut off partway.
//
// Conflating them is how a half-written representment reaches a reviewer
// looking finished. Nothing downstream may treat a Result as an answer without
// checking this field.
type Halt string

const (
	HaltCompleted   Halt = "completed"
	HaltTurnCeiling Halt = "turn_ceiling"
	HaltBudget      Halt = "budget_exceeded"
	HaltTruncated   Halt = "output_truncated"
	HaltStopReason  Halt = "unexpected_stop_reason"
)

// Done reports whether the model reached its own end, as opposed to being
// stopped by one of the ceilings.
func (h Halt) Done() bool { return h == HaltCompleted }

// Budget bounds one run. Both ceilings are required: turns alone does not stop
// a model that reads enormous results, and cost alone does not stop one that
// ping-pongs cheaply forever.
type Budget struct {
	MaxTurns      int
	MaxCostMicros int64

	// MaxTokens caps a single response. It is what bounds the overshoot
	// described on the cost check below, so it is part of the budget rather
	// than an incidental request field.
	MaxTokens int
}

// ---------------------------------------------------------------------------
// trace
// ---------------------------------------------------------------------------

// ToolCall is one tool invocation, as recorded for the audit trail.
//
// The arguments are kept verbatim. They are ids, states and merchant
// references - no cardholder data reaches a tool input - and being able to
// replay exactly what was asked is the point of keeping a trail at all.
type ToolCall struct {
	Name        string          `json:"name"`
	Input       json.RawMessage `json:"input"`
	ResultBytes int             `json:"result_bytes"`
	IsError     bool            `json:"is_error"`
	// Rule is set when the call was refused: which rule refused it. Empty on
	// a call that ran. This is what lets a session report say "refusals by
	// rule" instead of "some errors".
	Rule    string        `json:"rule,omitempty"`
	Latency time.Duration `json:"latency_ns"`
}

type Turn struct {
	Index      int           `json:"index"`
	StopReason string        `json:"stop_reason"`
	Usage      Usage         `json:"usage"`
	CostMicros int64         `json:"cost_micros"`
	Latency    time.Duration `json:"latency_ns"`
	ToolCalls  []ToolCall    `json:"tool_calls,omitempty"`
}

// Result is everything the caller needs to decide what happened, including
// whether to trust the text.
type Result struct {
	Text       string    `json:"text"`
	Halt       Halt      `json:"halt"`
	StopReason string    `json:"stop_reason"`
	Turns      []Turn    `json:"turns"`
	Usage      Usage     `json:"usage"`
	CostMicros int64     `json:"cost_micros"`
	Messages   []Message `json:"-"`
}

// ---------------------------------------------------------------------------
// the loop
// ---------------------------------------------------------------------------

// maxParallelTools bounds the fan-out when a model asks for several tools at
// once. They are reads against one pool, so this is about not handing the
// database a surprise, not about contention between the calls themselves.
const maxParallelTools = 4

// Loop runs a model over a tool set until it finishes or hits a ceiling.
type Loop struct {
	completer Completer
	tools     *Registry
	pricing   Pricing
	budget    Budget
	model     string
}

func NewLoop(completer Completer, tools *Registry, model string, pricing Pricing, budget Budget) *Loop {
	return &Loop{completer: completer, tools: tools, pricing: pricing, budget: budget, model: model}
}

// Run drives the conversation.
//
// The shape is the whole of an agent: send, look at stop_reason, run whatever
// tools were asked for, send the results back, repeat. Everything else in this
// function is about stopping - which is the part that is actually hard, and the
// part that separates a demo from something you would let near money.
func (l *Loop) Run(ctx context.Context, system string, prompt string) (Result, error) {
	messages := []Message{{
		Role:    "user",
		Content: []ContentBlock{{Type: "text", Text: prompt}},
	}}

	result := Result{Halt: HaltTurnCeiling}

	for turn := 0; turn < l.budget.MaxTurns; turn++ {
		if err := ctx.Err(); err != nil {
			return result, err
		}

		// Checked before the call, not after, so the budget is never knowingly
		// exceeded. It can still be overshot by one response, because the price
		// of a call is not known until it returns - which is what MaxTokens is
		// for: it bounds how large that last overshoot can be.
		if result.CostMicros >= l.budget.MaxCostMicros {
			result.Halt = HaltBudget
			return result, nil
		}

		started := time.Now()
		response, err := l.completer.Complete(ctx, Request{
			Model:     l.model,
			System:    system,
			Messages:  messages,
			Tools:     l.tools.Tools(),
			MaxTokens: l.budget.MaxTokens,
			// The system prompt and the tool schemas are the same on every
			// turn; only the conversation below them grows.
			CacheSystem: true,
		})
		if err != nil {
			return result, fmt.Errorf("turn %d: %w", turn, err)
		}

		record := Turn{
			Index:      turn,
			StopReason: response.StopReason,
			Usage:      response.Usage,
			CostMicros: l.pricing.cost(response.Usage),
			Latency:    time.Since(started),
		}

		messages = append(messages, Message{Role: "assistant", Content: response.Content})
		result.Text = textOf(response.Content)
		result.StopReason = response.StopReason

		switch response.StopReason {
		case "tool_use":
			uses := toolUsesIn(response.Content)
			if len(uses) == 0 {
				// The API said tools were requested and sent none. Continuing
				// would send an empty tool_result array and get the same answer
				// back, forever.
				result.Halt = HaltStopReason
				result.StopReason = "tool_use_without_tool_use_block"
				l.finish(&result, record)
				return result, nil
			}

			results, calls, err := l.runTools(ctx, uses)
			record.ToolCalls = calls
			if err != nil {
				l.finish(&result, record)
				return result, err
			}

			messages = append(messages, Message{Role: "user", Content: results})
			l.finish(&result, record)

		case "end_turn", "stop_sequence":
			result.Halt = HaltCompleted
			l.finish(&result, record)
			result.Messages = messages
			return result, nil

		case "max_tokens":
			// A cut-off response is not a short answer, it is an unfinished
			// one, and the text it produced may stop mid-sentence. Marked so
			// that nothing downstream reads it as a completed draft.
			result.Halt = HaltTruncated
			l.finish(&result, record)
			result.Messages = messages
			return result, nil

		default:
			// refusal, pause_turn, or something added to the API after this was
			// written. Recorded verbatim rather than guessed at.
			result.Halt = HaltStopReason
			l.finish(&result, record)
			result.Messages = messages
			return result, nil
		}
	}

	result.Messages = messages
	return result, nil
}

func (l *Loop) finish(result *Result, record Turn) {
	result.Turns = append(result.Turns, record)
	result.Usage.Add(record.Usage)
	result.CostMicros += record.CostMicros
}

// runTools executes one turn's worth of tool calls.
//
// Results are written into a slice by index rather than appended from the
// goroutines. Order does not affect correctness - each tool_result carries the
// tool_use_id it answers - but a trace whose order changes between runs cannot
// be diffed, and the eval set in step 7 depends on being able to diff it.
func (l *Loop) runTools(ctx context.Context, uses []ToolUse) ([]ContentBlock, []ToolCall, error) {
	blocks := make([]ContentBlock, len(uses))
	calls := make([]ToolCall, len(uses))

	group, groupCtx := errgroup.WithContext(ctx)
	group.SetLimit(maxParallelTools)

	for i, use := range uses {
		group.Go(func() error {
			started := time.Now()
			out, err := l.tools.Run(groupCtx, use)
			if err != nil {
				return err
			}
			blocks[i] = out.block()
			calls[i] = ToolCall{
				Name:        use.Name,
				Input:       use.Input,
				ResultBytes: len(out.Content),
				IsError:     out.IsError,
				Rule:        out.Rule,
				Latency:     time.Since(started),
			}
			return nil
		})
	}

	if err := group.Wait(); err != nil {
		return nil, calls, err
	}
	return blocks, calls, nil
}

func (r ToolResult) block() ContentBlock {
	return ContentBlock{
		Type:      "tool_result",
		ToolUseID: r.ToolUseID,
		Content:   r.Content,
		IsError:   r.IsError,
	}
}

func toolUsesIn(content []ContentBlock) []ToolUse {
	var uses []ToolUse
	for _, block := range content {
		if block.Type == "tool_use" {
			uses = append(uses, ToolUse{ID: block.ID, Name: block.Name, Input: block.Input})
		}
	}
	return uses
}

// textOf joins the text blocks of a response.
//
// A response can carry text alongside its tool calls - the model narrating what
// it is about to look up - so this is not only the final answer. The loop keeps
// the most recent one, and Halt says whether it is finished.
func textOf(content []ContentBlock) string {
	var joined string
	for _, block := range content {
		if block.Type == "text" {
			if joined != "" {
				joined += "\n"
			}
			joined += block.Text
		}
	}
	return joined
}
