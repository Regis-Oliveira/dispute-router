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
// Everything crossing the tool boundary costs context: a tool that answers
// with 200 KB of JSON has spent the budget the reasoning needed. A full 50-row
// list lands around 12 KB, so this is headroom rather than a limit anyone
// should meet. Meeting it means something is wrong with the question, which is
// why the answer says so instead of quietly cutting the JSON in half.
const maxResultBytes = 32 * 1024

// ---------------------------------------------------------------------------
// wire types
// ---------------------------------------------------------------------------

// These are the Messages API tool-use blocks, written out rather than
// imported: the wire types are kept SDK-free so another completer can share
// them, and so this package is testable without a network call or an API key,
// which is what makes the eval set possible.

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

// ToolResult is what one tool call produced; block turns it into the
// tool_result content block sent back on the next turn.
//
// Content is a string rather than a nested block array: every one of these
// tools answers with JSON, and a string is what the API accepts for that.
type ToolResult struct {
	ToolUseID string
	Content   string
	IsError   bool

	// Rule names why a call was refused, for the trace and never for the
	// wire. A refusal the model reads is prose; a refusal an operator counts
	// is a rule id, and "refusals by rule" cannot be totalled from prose. The
	// same reason every verifier finding carries a check name.
	Rule Rule
}

// Rule names why a tool call was refused. A closed set, so a report can count
// them across sessions; the loop's own stops are named by Halt.
//
// The zero value means the call was not refused, which is why every refusal
// path names one explicitly.
type Rule string

const (
	// RuleUnknownTool is a name the registry does not hold. Answered rather
	// than fatal: a model that guessed can pick a real name if it is told
	// which exist.
	RuleUnknownTool Rule = "unknown_tool"

	// RuleInvalidArguments is input the tool could not decode or would not
	// accept.
	RuleInvalidArguments Rule = "invalid_arguments"

	// RuleNotFound is a well-formed request for something that is not there.
	RuleNotFound Rule = "not_found"

	// RuleResultTooLarge is an answer over maxResultBytes. Refused whole
	// rather than cut, because half a JSON document is not a smaller answer.
	RuleResultTooLarge Rule = "result_too_large"
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

// NewRegistry exposes the catalog in internal/disputetools as tools.
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
// A tool failure is usually a message, not a crash. A model that asks for a
// dispute that does not exist needs to be told so, in a form it can read and
// act on; killing the run instead throws away every token spent getting there.
// But a database that is down is not something a model can reason its way
// past, and handing it back as text just invites a retry loop that bills for
// nothing. So errors are sorted: the ones the model can act on come back in
// the ToolResult with IsError set, for the loop to hand to the next turn, and
// the returned error is fatal to the run.
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

	return ToolResult{ToolUseID: use.ID, Content: string(encoded)}, nil
}

// ruleFor says whether an error describes something about the request rather
// than something about the system, and names the rule when it does.
//
// The sentinels come from the packages that produce them - the store for a
// dispute that does not exist, the tool catalog for arguments that did not
// decode - so this stays a question about the domain instead of a guess about
// error text. Anything else is infrastructure, and not actionable.
func ruleFor(err error) (rule Rule, actionable bool) {
	switch {
	case errors.Is(err, disputetools.ErrInvalidArguments), errors.Is(err, api.ErrInvalidInput):
		return RuleInvalidArguments, true
	case errors.Is(err, api.ErrNotFound):
		return RuleNotFound, true
	}
	return "", false
}

func refuse(use ToolUse, rule Rule, reason string) ToolResult {
	return ToolResult{
		ToolUseID: use.ID,
		Content:   reason,
		IsError:   true,
		Rule:      rule,
	}
}
