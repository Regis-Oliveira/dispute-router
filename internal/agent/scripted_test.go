package agent

import (
	"context"
	"fmt"
)

// ScriptedCompleter is the in-package copy of agenttest.ScriptedCompleter.
//
// It exists because these tests are package agent - they reach unexported
// names throughout - and agenttest imports agent, so they cannot import it
// back. Keep the two in step; agenttest is the one other packages use.
type ScriptedCompleter struct {
	Responses []Response
	Requests  []Request
	calls     int
}

func (c *ScriptedCompleter) Complete(_ context.Context, req Request) (Response, error) {
	c.Requests = append(c.Requests, req)
	if c.calls >= len(c.Responses) {
		return Response{}, fmt.Errorf("scripted completer: no response for call %d (script has %d)",
			c.calls+1, len(c.Responses))
	}
	response := c.Responses[c.calls]
	c.calls++
	return response, nil
}

func (c *ScriptedCompleter) Calls() int { return c.calls }

func Says(text string, usage Usage) Response {
	return Response{
		Content:    []ContentBlock{{Type: "text", Text: text}},
		StopReason: "end_turn",
		Usage:      usage,
	}
}

func Calls(usage Usage, uses ...ToolUse) Response {
	content := make([]ContentBlock, 0, len(uses))
	for _, use := range uses {
		content = append(content, ContentBlock{
			Type: "tool_use", ID: use.ID, Name: use.Name, Input: use.Input,
		})
	}
	return Response{Content: content, StopReason: "tool_use", Usage: usage}
}

func Truncated(partial string, usage Usage) Response {
	return Response{
		Content:    []ContentBlock{{Type: "text", Text: partial}},
		StopReason: "max_tokens",
		Usage:      usage,
	}
}
