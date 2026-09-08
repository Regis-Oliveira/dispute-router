package agent

import (
	"context"
	"fmt"
)

// ScriptedCompleter answers from a fixed list instead of calling a model.
//
// It lives here rather than in a _test.go file because it is not only test
// scaffolding: the eval set is a separate package, and replaying recorded
// responses is how those runs stay deterministic and free. Every real call
// spends money, so the ability to exercise the loop without one is a property
// of the design, not a convenience.
//
// Complete is called serially by the loop, so there is no locking here. That is
// a statement about the loop, not an assumption about callers - anything that
// drives several runs at once needs one of these per run.
type ScriptedCompleter struct {
	// Responses are returned in order, one per turn.
	Responses []Response

	// Requests records what the loop actually sent, which is most of what
	// there is to assert about a loop: that tool results came back attached to
	// the right ids, that the tool list was offered every turn, that the
	// history grew rather than being rewritten.
	Requests []Request

	calls int
}

func (c *ScriptedCompleter) Complete(_ context.Context, req Request) (Response, error) {
	c.Requests = append(c.Requests, req)

	if c.calls >= len(c.Responses) {
		// An exhausted script means the loop ran further than the test
		// expected. Erroring says so; repeating the last response would let a
		// test pass while describing behaviour nobody wrote down.
		return Response{}, fmt.Errorf("scripted completer: no response for call %d (script has %d)",
			c.calls+1, len(c.Responses))
	}

	response := c.Responses[c.calls]
	c.calls++
	return response, nil
}

// Calls is how many times the loop asked for a completion.
func (c *ScriptedCompleter) Calls() int { return c.calls }

// ---------------------------------------------------------------------------
// constructors for the shapes a script is usually made of
// ---------------------------------------------------------------------------

// Says is a finished answer.
func Says(text string, usage Usage) Response {
	return Response{
		Content:    []ContentBlock{{Type: "text", Text: text}},
		StopReason: "end_turn",
		Usage:      usage,
	}
}

// Calls is a turn that asks for one or more tools.
func Calls(usage Usage, uses ...ToolUse) Response {
	content := make([]ContentBlock, 0, len(uses))
	for _, use := range uses {
		content = append(content, ContentBlock{
			Type: "tool_use", ID: use.ID, Name: use.Name, Input: use.Input,
		})
	}
	return Response{Content: content, StopReason: "tool_use", Usage: usage}
}

// Truncated is a response the model did not get to finish.
func Truncated(partial string, usage Usage) Response {
	return Response{
		Content:    []ContentBlock{{Type: "text", Text: partial}},
		StopReason: "max_tokens",
		Usage:      usage,
	}
}
