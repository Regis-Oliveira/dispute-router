// Package agent runs a model in a loop over the dispute read model.
//
// This file is one half of that: the tool boundary. It puts the tools from
// internal/disputetools into the shape the Messages API expects, dispatches a
// tool_use block to the right handler, and turns whatever comes back into a
// tool_result block.
//
// Two rules shape the whole file.
//
// The first is that a tool failure is usually a message, not a crash. A model
// that asks for a dispute that does not exist needs to be told so, in a form it
// can read and act on; killing the run instead throws away every token spent
// getting there. But a database that is down is not something a model can
// reason its way past, and handing it back as text just invites a retry loop
// that bills for nothing. So errors are sorted: the ones the model can act on
// become tool_result blocks, and the ones it cannot abort the run.
//
// The second is that everything crossing this boundary costs context. A tool
// that answers with 200 KB of JSON has not been helpful - it has spent the
// budget the reasoning needed. Hence the byte ceiling below, and hence the fact
// that it refuses out loud rather than truncating into JSON that no longer
// parses.
package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/regisoliveira/dispute-router/internal/api"
	"github.com/regisoliveira/dispute-router/internal/disputetools"
)

// maxResultBytes caps one tool result, roughly eight thousand tokens.
//
// A full 50-row list lands around 12 KB, so this is headroom rather than a
// limit anyone should meet. Meeting it means something is wrong with the
// question, which is why the answer says so instead of quietly cutting the
// JSON in half.
const maxResultBytes = 32 * 1024

// ---------------------------------------------------------------------------
// wire types
// ---------------------------------------------------------------------------

// These are the Messages API tool-use blocks, written out rather than imported.
//
// The model is reached through Bedrock's InvokeModel, whose body is this JSON
// verbatim, and the AWS SDK is already a dependency - so a second vendor SDK
// would buy nothing here. It also keeps this package testable without a network
// call or an API key, which is what makes the eval set in step 7 possible.

// Tool is one entry in the request's "tools" array.
type Tool struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"input_schema"`
}

// ToolUse is a tool_use content block from the model.
type ToolUse struct {
	ID    string          `json:"id"`
	Name  string          `json:"name"`
	Input json.RawMessage `json:"input"`
}

// ToolResult is the tool_result content block sent back on the next turn.
//
// Content is a string rather than a nested block array: every one of these
// tools answers with JSON, and a string is what the API accepts for that.
type ToolResult struct {
	Type      string `json:"type"`
	ToolUseID string `json:"tool_use_id"`
	Content   string `json:"content"`
	IsError   bool   `json:"is_error,omitempty"`

	// Rule names why a call was refused, for the trace and never for the
	// wire. A refusal the model reads is prose; a refusal an operator counts
	// is a rule id, and "refusals by rule" cannot be totalled from prose. The
	// same reason every verifier finding carries a check name.
	Rule string `json:"-"`
}

// The rules a tool call can be refused under. A closed set, so a report can
// count them across sessions; the loop's own stops are named by Halt.
const (
	RuleUnknownTool      = "unknown_tool"
	RuleInvalidArguments = "invalid_arguments"
	RuleNotFound         = "not_found"
	RuleResultTooLarge   = "result_too_large"
)

// ---------------------------------------------------------------------------
// registry
// ---------------------------------------------------------------------------

// Registry is the set of tools this agent may call, and the only way it can
// call them.
//
// The map is the allowlist. A name that is not in it is not dispatched - there
// is no fallback, no prefix match, no "close enough". That matters more than it
// looks: the name arrives from the model, and the model's output is shaped in
// part by text other people wrote.
type Registry struct {
	byName map[string]disputetools.Definition
	specs  []Tool
}

func NewRegistry(store *api.Store) *Registry {
	catalog := disputetools.New(store).Catalog()

	r := &Registry{
		byName: make(map[string]disputetools.Definition, len(catalog)),
		specs:  make([]Tool, 0, len(catalog)),
	}
	for _, def := range catalog {
		r.byName[def.Name] = def
		r.specs = append(r.specs, Tool{
			Name:        def.Name,
			Description: def.Description,
			InputSchema: def.InputSchema,
		})
	}
	return r
}

// Tools is the "tools" array for the request, in catalog order.
func (r *Registry) Tools() []Tool { return r.specs }

// Names is for the audit trail: the exact tool surface a run was given.
//
// Recorded per run rather than assumed from the code, because the code changes
// and the audit trail has to stay true about the run it describes.
func (r *Registry) Names() []string {
	names := make([]string, 0, len(r.specs))
	for _, spec := range r.specs {
		names = append(names, spec.Name)
	}
	return names
}

// Run executes one tool_use block.
//
// The returned error is fatal to the run. Anything the model can do something
// about comes back in the ToolResult with IsError set, so the loop can hand it
// straight to the next turn.
func (r *Registry) Run(ctx context.Context, use ToolUse) (ToolResult, error) {
	def, known := r.byName[use.Name]
	if !known {
		// Worth answering rather than aborting: a model that guessed a tool
		// name can pick a real one if it is told which exist. Aborting throws
		// away the run over a typo.
		return refuse(use, RuleUnknownTool, fmt.Sprintf("no tool named %q; available tools are %v", use.Name, r.Names())), nil
	}

	value, err := def.Invoke(ctx, use.Input)
	if err != nil {
		// Infrastructure is not something the model can reason past, and
		// telling it so only buys a retry that bills for nothing.
		if ctx.Err() != nil {
			return ToolResult{}, fmt.Errorf("tool %s: %w", use.Name, ctx.Err())
		}
		rule, actionable := ruleFor(err)
		if !actionable {
			return ToolResult{}, fmt.Errorf("tool %s: %w", use.Name, err)
		}
		return refuse(use, rule, err.Error()), nil
	}

	// Compact, not indented. Indentation is easier on a human eye and costs
	// tokens for a reader that does not have one.
	encoded, err := json.Marshal(value)
	if err != nil {
		return ToolResult{}, fmt.Errorf("tool %s: encoding result: %w", use.Name, err)
	}

	if len(encoded) > maxResultBytes {
		// Refused, not truncated. Half a JSON document is not a smaller answer,
		// it is an unparseable one, and a model handed one will either fail to
		// read it or - worse - read the part it got and treat it as the whole.
		return refuse(use, RuleResultTooLarge, fmt.Sprintf(
			"result is %d bytes, over the %d byte limit; narrow the request, for example with a smaller limit or a tighter filter",
			len(encoded), maxResultBytes)), nil
	}

	return ToolResult{Type: "tool_result", ToolUseID: use.ID, Content: string(encoded)}, nil
}

// ruleFor says whether an error describes something about the request rather
// than something about the system, and names the rule when it does.
//
// The sentinels come from the packages that produce them - the store for a
// dispute that does not exist, the tool catalog for arguments that did not
// decode - so this stays a question about the domain instead of a guess about
// error text. Anything else is infrastructure, and not actionable.
func ruleFor(err error) (rule string, actionable bool) {
	switch {
	case errors.Is(err, disputetools.ErrInvalidArguments), errors.Is(err, api.ErrInvalidInput):
		return RuleInvalidArguments, true
	case errors.Is(err, api.ErrNotFound):
		return RuleNotFound, true
	}
	return "", false
}

func refuse(use ToolUse, rule, reason string) ToolResult {
	return ToolResult{
		Type:      "tool_result",
		ToolUseID: use.ID,
		Content:   reason,
		IsError:   true,
		Rule:      rule,
	}
}
