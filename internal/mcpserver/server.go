// Package mcpserver exposes the dispute read model over the Model Context
// Protocol, so an assistant can look at the platform instead of guessing.
//
// The whole design question here is not "what can I expose" but "what should
// I". An MCP server is a set of capabilities handed to something that decides
// on its own when to use them, prompted partly by text it was given. Every tool
// below is read-only, and the section at the bottom of this file lists what was
// deliberately left out and why - that list is the more interesting half.
package mcpserver

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/regisoliveira/dispute-router/internal/api"
)

// maxRows caps every list.
//
// Not a performance limit - a context limit. A model that asks for ten thousand
// disputes gets a truncated transcript and worse answers, and it has no way to
// know that happened. The cap is stated in each tool description so the model
// can page deliberately instead of being silently cut off.
const maxRows = 50

type Server struct {
	store *api.Store
}

func New(store *api.Store) *Server {
	return &Server{store: store}
}

// maskEmail keeps the shape of an address without handing over the address.
//
// A customer's email is theirs, not the platform's, and it has no bearing on
// whether a dispute is winnable. It appears here only because seeing that two
// disputes share a customer is genuinely useful - and customer_ref already
// carries that, so the address itself never needs to leave the database.
func maskEmail(address string) string {
	at := strings.LastIndex(address, "@")
	if at <= 0 {
		return "***"
	}
	local := address[:at]
	if len(local) > 1 {
		local = local[:1] + strings.Repeat("*", 3)
	}
	return local + address[at:]
}

// ---------------------------------------------------------------------------
// list_disputes
// ---------------------------------------------------------------------------

type ListDisputesInput struct {
	State     string `json:"state,omitempty" jsonschema:"Comma-separated states: received, resolving, represented, refunded, won, lost, expired"`
	Kind      string `json:"kind,omitempty" jsonschema:"alert or chargeback. Alerts are pre-dispute warnings with hours to act; chargebacks are already filed"`
	Merchant  string `json:"merchant,omitempty" jsonschema:"Merchant external id, for example mrc_northwind"`
	DueWithin string `json:"due_within,omitempty" jsonschema:"Only open disputes whose deadline falls inside this window, for example 24h. Already-overdue disputes match too and are flagged with overdue=true"`
	OpenOnly  bool   `json:"open_only,omitempty" jsonschema:"Only disputes still awaiting a decision"`
	Limit     int    `json:"limit,omitempty" jsonschema:"How many to return, 1 to 50. Defaults to 20"`
}

type DisputeSummary struct {
	ID              int64  `json:"id"`
	Reference       string `json:"reference"`
	Merchant        string `json:"merchant"`
	Kind            string `json:"kind"`
	State           string `json:"state"`
	ReasonCode      string `json:"reason_code"`
	AmountMinor     int64  `json:"amount_minor"`
	Currency        string `json:"currency"`
	HoursToDeadline int    `json:"hours_to_deadline"`
	// Overdue is stated rather than left as the sign of the number above. A
	// negative hours_to_deadline is easy to skim past; "overdue": true is not,
	// and the difference is whether the reader thinks there is time left.
	Overdue  bool   `json:"overdue"`
	OpenedAt string `json:"opened_at"`
}

type ListDisputesOutput struct {
	Disputes []DisputeSummary `json:"disputes"`
	Total    int64            `json:"total_matching"`
	Note     string           `json:"note,omitempty"`
}

func (s *Server) listDisputes(ctx context.Context, _ *mcp.CallToolRequest, in ListDisputesInput) (*mcp.CallToolResult, ListDisputesOutput, error) {
	limit := in.Limit
	if limit <= 0 {
		limit = 20
	}
	if limit > maxRows {
		limit = maxRows
	}

	filters := api.Filters{
		States:    splitCSV(in.State),
		Kinds:     splitCSV(in.Kind),
		Merchants: splitCSV(in.Merchant),
		OpenOnly:  in.OpenOnly,
		Sort:      "deadline_at",
		Desc:      false,
		Limit:     limit,
	}

	if in.DueWithin != "" {
		window, err := time.ParseDuration(in.DueWithin)
		if err != nil {
			return nil, ListDisputesOutput{}, fmt.Errorf("due_within must be a duration such as 24h: %w", err)
		}
		filters.DueWithin = &window
		filters.OpenOnly = true
	}

	list, err := s.store.ListDisputes(ctx, filters)
	if err != nil {
		return nil, ListDisputesOutput{}, err
	}

	out := ListDisputesOutput{Disputes: make([]DisputeSummary, 0, len(list.Rows)), Total: list.Page.Total}
	for _, row := range list.Rows {
		out.Disputes = append(out.Disputes, DisputeSummary{
			ID:              row.ID,
			Reference:       row.ExternalID,
			Merchant:        row.MerchantName,
			Kind:            row.Kind,
			State:           row.State,
			ReasonCode:      row.ReasonCode,
			AmountMinor:     row.Amount.AmountMinor,
			Currency:        row.Amount.Currency,
			HoursToDeadline: int(row.SecondsToDeadline / 3600),
			Overdue:         row.SecondsToDeadline < 0 && row.ResolvedAt == nil,
			OpenedAt:        row.OpenedAt.UTC().Format(time.RFC3339),
		})
	}

	// Said explicitly rather than left for the model to infer from a count it
	// cannot see. Silent truncation is how an assistant concludes "there are
	// only 20" and states it with confidence.
	if list.Page.Total > int64(len(out.Disputes)) {
		out.Note = fmt.Sprintf("showing %d of %d matches; narrow the filters to see the rest",
			len(out.Disputes), list.Page.Total)
	}
	return nil, out, nil
}

// ---------------------------------------------------------------------------
// get_dispute
// ---------------------------------------------------------------------------

type GetDisputeInput struct {
	ID int64 `json:"id" jsonschema:"The dispute's numeric id, from list_disputes"`
}

type LedgerLine struct {
	Entry       string `json:"entry"`
	Account     string `json:"account"`
	Direction   string `json:"direction"`
	AmountMinor int64  `json:"amount_minor"`
}

type HistoryLine struct {
	At    string `json:"at"`
	From  string `json:"from,omitempty"`
	To    string `json:"to"`
	Actor string `json:"actor"`
}

type GetDisputeOutput struct {
	ID              int64         `json:"id"`
	Reference       string        `json:"reference"`
	Merchant        string        `json:"merchant"`
	Kind            string        `json:"kind"`
	State           string        `json:"state"`
	ReasonCode      string        `json:"reason_code"`
	CardNetwork     string        `json:"card_network"`
	AmountMinor     int64         `json:"amount_minor"`
	Currency        string        `json:"currency"`
	OriginalCharge  int64         `json:"original_charge_minor"`
	RefundedMinor   int64         `json:"refunded_minor"`
	HoursToDeadline int           `json:"hours_to_deadline"`
	Overdue         bool          `json:"overdue"`
	OpenedAt        string        `json:"opened_at"`
	ChargedAt       string        `json:"charged_at"`
	Descriptor      string        `json:"statement_descriptor"`
	CardLast4       string        `json:"card_last4"`
	CustomerRef     string        `json:"customer_ref"`
	CustomerEmail   string        `json:"customer_email_masked"`
	History         []HistoryLine `json:"history"`
	Ledger          []LedgerLine  `json:"ledger"`
}

func (s *Server) getDispute(ctx context.Context, _ *mcp.CallToolRequest, in GetDisputeInput) (*mcp.CallToolResult, GetDisputeOutput, error) {
	d, err := s.store.Dispute(ctx, in.ID)
	if err != nil {
		return nil, GetDisputeOutput{}, err
	}

	out := GetDisputeOutput{
		ID: d.ID, Reference: d.ExternalID, Merchant: d.MerchantName,
		Kind: d.Kind, State: d.State, ReasonCode: d.ReasonCode, CardNetwork: d.CardNetwork,
		AmountMinor: d.Amount.AmountMinor, Currency: d.Amount.Currency,
		OriginalCharge: d.OriginalAmount.AmountMinor, RefundedMinor: d.RefundedMinor,
		HoursToDeadline: int(d.SecondsToDeadline / 3600),
		Overdue:         d.SecondsToDeadline < 0 && d.ResolvedAt == nil,
		OpenedAt:        d.OpenedAt.UTC().Format(time.RFC3339),
		ChargedAt:       d.CapturedAt.UTC().Format(time.RFC3339),
		Descriptor:      d.Descriptor,
		CardLast4:       d.CardLast4,
		CustomerRef:     d.CustomerRef,
		CustomerEmail:   maskEmail(d.CustomerEmail),
	}

	for _, e := range d.Events {
		line := HistoryLine{At: e.OccurredAt.UTC().Format(time.RFC3339), To: e.ToState, Actor: e.Actor}
		if e.FromState != nil {
			line.From = *e.FromState
		}
		out.History = append(out.History, line)
	}
	for _, p := range d.LedgerPostings {
		out.Ledger = append(out.Ledger, LedgerLine{
			Entry: p.Kind, Account: p.Account, Direction: p.Direction,
			AmountMinor: p.Amount.AmountMinor,
		})
	}
	return nil, out, nil
}

// ---------------------------------------------------------------------------
// get_customer_history
// ---------------------------------------------------------------------------

type CustomerHistoryInput struct {
	Merchant    string `json:"merchant" jsonschema:"Merchant external id, for example mrc_northwind"`
	CustomerRef string `json:"customer_ref" jsonschema:"Customer reference from get_dispute"`
	Limit       int    `json:"limit,omitempty" jsonschema:"How many prior disputes, 1 to 50. Defaults to 20"`
}

type CustomerHistoryOutput struct {
	Disputes []api.CustomerHistoryRow `json:"disputes"`
	Count    int                      `json:"count"`
}

func (s *Server) customerHistory(ctx context.Context, _ *mcp.CallToolRequest, in CustomerHistoryInput) (*mcp.CallToolResult, CustomerHistoryOutput, error) {
	limit := in.Limit
	if limit <= 0 {
		limit = 20
	}
	if limit > maxRows {
		limit = maxRows
	}

	rows, err := s.store.CustomerHistory(ctx, in.Merchant, in.CustomerRef, limit)
	if err != nil {
		return nil, CustomerHistoryOutput{}, err
	}
	return nil, CustomerHistoryOutput{Disputes: rows, Count: len(rows)}, nil
}

// ---------------------------------------------------------------------------
// queue_summary
// ---------------------------------------------------------------------------

type QueueSummaryInput struct {
	Merchant string `json:"merchant,omitempty" jsonschema:"Optional merchant external id to narrow the summary"`
}

type QueueSummaryOutput struct {
	ByState   []api.StateCount     `json:"by_state"`
	AtRisk    []api.Exposure       `json:"money_at_risk"`
	Deadlines []api.DeadlineBucket `json:"deadline_buckets"`
}

func (s *Server) queueSummary(ctx context.Context, _ *mcp.CallToolRequest, in QueueSummaryInput) (*mcp.CallToolResult, QueueSummaryOutput, error) {
	summary, err := s.store.Summary(ctx, api.Filters{Merchants: splitCSV(in.Merchant)})
	if err != nil {
		return nil, QueueSummaryOutput{}, err
	}
	return nil, QueueSummaryOutput{
		ByState: summary.States, AtRisk: summary.OpenValue, Deadlines: summary.Deadlines,
	}, nil
}

// ---------------------------------------------------------------------------
// registration
// ---------------------------------------------------------------------------

// readOnly marks every tool here. The hint is advisory - it does not enforce
// anything - but it is what a client shows a user when asking whether to allow
// a call, and being able to say "all of them are read-only" truthfully is the
// point of the design.
func readOnly(title string) *mcp.ToolAnnotations {
	return &mcp.ToolAnnotations{Title: title, ReadOnlyHint: true, IdempotentHint: true}
}

// Register wires the tools onto an MCP server.
//
// Tool descriptions are prompt, not documentation. They are the only thing the
// model reads before deciding whether a tool is the right one, so they say what
// the tool is FOR and what its answer means - not just what it returns.
func (s *Server) Register(server *mcp.Server) {
	mcp.AddTool(server, &mcp.Tool{
		Name: "list_disputes",
		Description: "Find disputes. Use this to answer questions about what is in the queue, " +
			"what is due soon, or which disputes match a state or merchant. Results are ordered " +
			"by deadline, soonest first, and capped at 50 - narrow the filters rather than " +
			"asking for more. Check the overdue flag before saying a dispute still has time: " +
			"a due_within window includes disputes whose deadline has already passed, and those " +
			"carry a negative hours_to_deadline.",
		Annotations: readOnly("List disputes"),
	}, s.listDisputes)

	mcp.AddTool(server, &mcp.Tool{
		Name: "get_dispute",
		Description: "Everything known about one dispute: the amounts, the original charge, " +
			"the full state history with who caused each transition, and the ledger entries it " +
			"produced. Use this before reasoning about a specific dispute - the list view " +
			"deliberately omits most of it.",
		Annotations: readOnly("Get one dispute"),
	}, s.getDispute)

	mcp.AddTool(server, &mcp.Tool{
		Name: "get_customer_history",
		Description: "Prior disputes filed by the same customer at the same merchant. The " +
			"single most useful signal when judging whether a dispute is worth fighting: a " +
			"first-time claim reads very differently from a fifth.",
		Annotations: readOnly("Customer dispute history"),
	}, s.customerHistory)

	mcp.AddTool(server, &mcp.Tool{
		Name: "queue_summary",
		Description: "Counts by state and kind, money currently at risk per currency, and how " +
			"the open deadlines are distributed. Use this for questions about the queue as a " +
			"whole rather than about any one dispute.",
		Annotations: readOnly("Queue summary"),
	}, s.queueSummary)
}

func splitCSV(raw string) []string {
	if strings.TrimSpace(raw) == "" {
		return nil
	}
	parts := strings.Split(raw, ",")
	out := make([]string, 0, len(parts))
	for _, part := range parts {
		if trimmed := strings.TrimSpace(part); trimmed != "" {
			out = append(out, trimmed)
		}
	}
	return out
}

// What is deliberately NOT here, and why
//
// This list is the design. Every entry was easy to add and is missing on
// purpose.
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
