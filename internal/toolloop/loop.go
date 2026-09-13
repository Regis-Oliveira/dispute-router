// Package toolloop drives a model over a set of read-only tools until it
// finishes or hits a ceiling.
//
// The shape is the whole of an agent: send, look at stop_reason, run whatever
// tools were asked for, send the results back, repeat. Everything else is about
// stopping - Budget bounds the turns and the spend, Halt says which ceiling was
// hit - and stopping is the part that separates a demo from something you would
// let near money. Turn and ToolCall are the audit trail of one run: what was
// asked, what answered, what it cost.
//
// The driving half is general: it knows an llm.Completer, a registry of tools
// and a price list. The registry it is handed is not - NewRegistry binds it to
// the four dispute tools in internal/disputetools, and ruleFor sorts their
// errors into the ones a model can act on. Splitting that off would leave a package
// whose only content is a constructor, so the line is drawn here instead and
// named: everything above Registry is reusable, Registry itself is this
// platform's.
//
// cmd/ask runs it. The representment flow in internal/agent deliberately does
// not, because two judges need one record and a generator that can fetch could
// cite something the verifier never saw.
package toolloop

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"golang.org/x/sync/errgroup"

	"github.com/regisoliveira/dispute-router/internal/llm"
)

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

// Budget bounds one run. Both ceilings are wanted: turns alone does not stop
// a model that reads enormous results, and cost alone does not stop one that
// ping-pongs cheaply forever.
type Budget struct {
	MaxTurns int

	// MaxCostMicros is the ceiling on one run. Zero means no ceiling, the same
	// convention AssistantOptions and the eval runner use.
	//
	// It used to mean the opposite here - a zero halted the loop before its
	// first call - so the one field had two readings depending on which file
	// was looking at it. A zero read as "nothing may be spent" is the worse of
	// the two: it stops every run before anything is bought and looks exactly
	// like a budget that is working.
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
	Rule    Rule          `json:"rule,omitempty"`
	Latency time.Duration `json:"latency_ns"`
}

// Turn is one round trip of the loop, as recorded for the audit trail.
type Turn struct {
	Index      int           `json:"index"`
	StopReason string        `json:"stop_reason"`
	Usage      llm.Usage     `json:"usage"`
	CostMicros int64         `json:"cost_micros"`
	Latency    time.Duration `json:"latency_ns"`
	ToolCalls  []ToolCall    `json:"tool_calls,omitempty"`
}

// Result is everything the caller needs to decide what happened, including
// whether to trust the text.
type Result struct {
	Text       string        `json:"text"`
	Halt       Halt          `json:"halt"`
	StopReason string        `json:"stop_reason"`
	Turns      []Turn        `json:"turns"`
	Usage      llm.Usage     `json:"usage"`
	CostMicros int64         `json:"cost_micros"`
	Messages   []llm.Message `json:"-"`
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
	completer llm.Completer
	tools     *Registry
	pricing   llm.Pricing
	budget    Budget
	model     string
}

// NewLoop binds a loop to a completer, a tool set and the budget that bounds it.
func NewLoop(completer llm.Completer, tools *Registry, model string, pricing llm.Pricing, budget Budget) *Loop {
	return &Loop{completer: completer, tools: tools, pricing: pricing, budget: budget, model: model}
}

// Run drives the conversation.
//
// The shape is the whole of an agent: send, look at stop_reason, run whatever
// tools were asked for, send the results back, repeat. Everything else in this
// function is about stopping - which is the part that is actually hard, and the
// part that separates a demo from something you would let near money.
func (l *Loop) Run(ctx context.Context, system string, prompt string) (Result, error) {
	messages := []llm.Message{{
		Role:    "user",
		Content: []llm.ContentBlock{{Type: "text", Text: prompt}},
	}}

	result := Result{Halt: HaltTurnCeiling}

	for turn := range l.budget.MaxTurns {
		if err := ctx.Err(); err != nil {
			return result, err
		}

		// Checked before the call, not after, so the budget is never knowingly
		// exceeded. It can still be overshot by one response, because the price
		// of a call is not known until it returns - which is what MaxTokens is
		// for: it bounds how large that last overshoot can be.
		if l.budget.MaxCostMicros > 0 && result.CostMicros >= l.budget.MaxCostMicros {
			result.Halt = HaltBudget
			return result, nil
		}

		started := time.Now()
		response, err := l.completer.Complete(ctx, llm.Request{
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
			CostMicros: l.pricing.Cost(response.Usage),
			Latency:    time.Since(started),
		}

		messages = append(messages, llm.Message{Role: "assistant", Content: response.Content})
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

			messages = append(messages, llm.Message{Role: "user", Content: results})
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
// be diffed, and the eval set depends on being able to diff it.
func (l *Loop) runTools(ctx context.Context, uses []llm.ToolUse) ([]llm.ContentBlock, []ToolCall, error) {
	blocks := make([]llm.ContentBlock, len(uses))
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
		// Only the calls that finished. The slice is filled by index, so a
		// failure partway left zero-value entries in it, and a zero ToolCall in
		// a trace reads as a nameless tool that answered instantly with
		// nothing - an audit trail describing calls that never happened.
		return nil, completed(calls), err
	}
	return blocks, calls, nil
}

// completed drops the entries no goroutine filled in. Safe to read here
// because every goroutine has returned by the time Wait does.
func completed(calls []ToolCall) []ToolCall {
	out := make([]ToolCall, 0, len(calls))
	for _, call := range calls {
		if call.Name != "" {
			out = append(out, call)
		}
	}
	return out
}

func (r ToolResult) block() llm.ContentBlock {
	return llm.ContentBlock{
		Type:      "tool_result",
		ToolUseID: r.ToolUseID,
		Content:   r.Content,
		IsError:   r.IsError,
	}
}

func toolUsesIn(content []llm.ContentBlock) []llm.ToolUse {
	var uses []llm.ToolUse
	for _, block := range content {
		if block.Type == "tool_use" {
			uses = append(uses, llm.ToolUse{ID: block.ID, Name: block.Name, Input: block.Input})
		}
	}
	return uses
}

// textOf joins the text blocks of a response.
//
// A response can carry text alongside its tool calls - the model narrating what
// it is about to look up - so this is not only the final answer. The loop keeps
// the most recent one, and Halt says whether it is finished.
func textOf(content []llm.ContentBlock) string {
	var b strings.Builder
	for _, block := range content {
		if block.Type != "text" {
			continue
		}
		if b.Len() > 0 {
			b.WriteString("\n")
		}
		b.WriteString(block.Text)
	}
	return b.String()
}
