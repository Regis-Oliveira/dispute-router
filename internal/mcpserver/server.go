// Package mcpserver puts the dispute tools behind the Model Context Protocol,
// so an assistant can look at the platform instead of guessing.
//
// There is almost nothing here, and that is the point. The tools themselves -
// what they return, what they mask, what their descriptions say - live in
// internal/disputetools, because they are also served over the Messages API to
// the agent in internal/agent. This file is the MCP envelope and nothing else:
// it maps a Definition onto mcp.AddTool and marks every one of them read-only.
//
// The whole design question for an MCP server is not "what can I expose" but
// "what should I". The list at the bottom of this file records what was
// deliberately left out - that list is the more interesting half.
package mcpserver

import (
	"context"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/regisoliveira/dispute-router/internal/api"
	"github.com/regisoliveira/dispute-router/internal/disputetools"
)

type Server struct {
	tools *disputetools.Set
}

func New(store *api.Store) *Server {
	return &Server{tools: disputetools.New(store)}
}

// readOnly marks every tool here. The hint is advisory - it does not enforce
// anything - but it is what a client shows a user when asking whether to allow
// a call, and being able to say "all of them are read-only" truthfully is the
// point of the design.
func readOnly(title string) *mcp.ToolAnnotations {
	return &mcp.ToolAnnotations{Title: title, ReadOnlyHint: true, IdempotentHint: true}
}

// Register wires the tools onto an MCP server.
//
// mcp.AddTool is generic over the input and output types, which is why this is
// four explicit calls rather than a loop over the catalog: the SDK derives each
// tool's schema from the Go types at the call site. It derives it with the same
// library and the same struct tags internal/disputetools uses for the Messages
// API, so the two transports describe the tools identically without either one
// owning the description.
func (s *Server) Register(server *mcp.Server) {
	mcp.AddTool(server, &mcp.Tool{
		Name:        "list_disputes",
		Description: disputetools.DescListDisputes,
		Annotations: readOnly("List disputes"),
	}, lift(s.tools.ListDisputes))

	mcp.AddTool(server, &mcp.Tool{
		Name:        "get_dispute",
		Description: disputetools.DescGetDispute,
		Annotations: readOnly("Get one dispute"),
	}, lift(s.tools.GetDispute))

	mcp.AddTool(server, &mcp.Tool{
		Name:        "get_customer_history",
		Description: disputetools.DescCustomerHistory,
		Annotations: readOnly("Customer dispute history"),
	}, lift(s.tools.CustomerHistory))

	mcp.AddTool(server, &mcp.Tool{
		Name:        "queue_summary",
		Description: disputetools.DescQueueSummary,
		Annotations: readOnly("Queue summary"),
	}, lift(s.tools.QueueSummary))
}

// lift adapts a plain handler to the MCP handler signature. The nil result
// tells the SDK to build the content block from the typed output itself.
func lift[In, Out any](fn func(context.Context, In) (Out, error)) mcp.ToolHandlerFor[In, Out] {
	return func(ctx context.Context, _ *mcp.CallToolRequest, in In) (*mcp.CallToolResult, Out, error) {
		out, err := fn(ctx, in)
		return nil, out, err
	}
}

// What is deliberately NOT here, and why
//
// This list is the design. Every entry was easy to add and is missing on
// purpose. It applies to the Messages API envelope in internal/agent too -
// there is one tool set, so there is one boundary.
//
//   - Any tool that writes. No refund, no state change, no evidence upload.
//     A model decides on its own when to call things, prompted partly by text
//     other people wrote. The blast radius of that should not include money.
//
//   - A generic run_query tool. It is the convenient thing to build and the
//     wrong thing to ship: it collapses every access decision into "can it
//     write SQL", and no amount of prompting re-establishes the boundary.
//
//   - Webhook signing secrets. Obvious, and worth stating: they are the one
//     credential in this system that lets somebody forge a dispute.
//
//   - Presigned evidence URLs. list_evidence would be genuinely useful, but a
//     presigned URL is a bearer credential with a TTL, and handing one to a
//     model puts it in a transcript that gets logged, replayed and pasted into
//     tickets. File names and sizes are the useful part; the URL is not.
//
//   - Unmasked customer emails. Theirs, not the platform's, and irrelevant to
//     whether a dispute is winnable. customer_ref already answers "is this the
//     same person".
//
// On prompt injection: in this schema the fields a model reads are mostly
// controlled vocabularies - reason codes, states, card networks - so the
// injection surface is thinner than it would be in a real platform, which
// would carry the cardholder's own free-text description of the claim. The
// mitigation that survives either way is the first entry above: read-only
// tools mean the worst a hostile string can do is produce a wrong answer,
// not a wrong refund.
