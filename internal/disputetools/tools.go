// Package disputetools is the four dispute tools, in plain Go, with no
// protocol attached.
//
// They are used through two different envelopes: the Model Context Protocol
// (internal/mcpserver) and the Messages API tool-use format
// (internal/agent). Neither of those is the tool - they are transports. What a
// tool actually is lives here: an input struct, an output struct, a
// description written to be read by a model, and a function between them.
//
// Keeping this separate is not tidiness. The description IS the prompt and the
// masking IS the privacy boundary, so a second copy of either is a second
// place for them to drift - and the version that drifts is always the one you
// are not currently looking at.
package disputetools

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/regisoliveira/dispute-router/internal/api"
)

// MaxRows caps every list.
//
// Not a performance limit - a context limit. A model that asks for ten thousand
// disputes gets a truncated transcript and worse answers, and it has no way to
// know that happened. The cap is stated in each tool description so the model
// can page deliberately instead of being silently cut off.
const MaxRows = 50

// defaultRows is what a caller gets when it does not say. Deliberately well
// under the cap: a tool that returns fifty rows to answer "what is due today"
// has spent context that the reasoning needed.
const defaultRows = 20

// Set is the four tools bound to a read model.
type Set struct {
	store *api.Store
}

func New(store *api.Store) *Set {
	return &Set{store: store}
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

func clampLimit(requested int) int {
	if requested <= 0 {
		return defaultRows
	}
	if requested > MaxRows {
		return MaxRows
	}
	return requested
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

// ---------------------------------------------------------------------------
// list_disputes
// ---------------------------------------------------------------------------

// DescListDisputes and its siblings are the prompt, not documentation. They
// are the only thing the model reads before deciding whether a tool is the
// right one, so they say what the tool is FOR and what its answer means - not
// just what it returns.
const DescListDisputes = "Find disputes. Use this to answer questions about what is in the queue, " +
	"what is due soon, or which disputes match a state or merchant. Results are ordered " +
	"by deadline, soonest first, and capped at 50 - narrow the filters rather than " +
	"asking for more. Check the overdue flag before saying a dispute still has time: " +
	"a due_within window includes disputes whose deadline has already passed, and those " +
	"carry a negative hours_to_deadline."

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

func (s *Set) ListDisputes(ctx context.Context, in ListDisputesInput) (ListDisputesOutput, error) {
	filters := api.Filters{
		States:    splitCSV(in.State),
		Kinds:     splitCSV(in.Kind),
		Merchants: splitCSV(in.Merchant),
		OpenOnly:  in.OpenOnly,
		Sort:      "deadline_at",
		Desc:      false,
		Limit:     clampLimit(in.Limit),
	}

	if in.DueWithin != "" {
		window, err := time.ParseDuration(in.DueWithin)
		if err != nil {
			return ListDisputesOutput{}, fmt.Errorf("due_within must be a duration such as 24h: %w", err)
		}
		filters.DueWithin = &window
		filters.OpenOnly = true
	}

	list, err := s.store.ListDisputes(ctx, filters)
	if err != nil {
		return ListDisputesOutput{}, err
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
	return out, nil
}

// ---------------------------------------------------------------------------
// get_dispute
// ---------------------------------------------------------------------------

const DescGetDispute = "Everything known about one dispute: the amounts, the original charge, " +
	"the full state history with who caused each transition, and the ledger entries it " +
	"produced. Use this before reasoning about a specific dispute - the list view " +
	"deliberately omits most of it."

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

func (s *Set) GetDispute(ctx context.Context, in GetDisputeInput) (GetDisputeOutput, error) {
	d, err := s.store.Dispute(ctx, in.ID)
	if err != nil {
		return GetDisputeOutput{}, err
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
	return out, nil
}

// ---------------------------------------------------------------------------
// get_customer_history
// ---------------------------------------------------------------------------

const DescCustomerHistory = "Prior disputes filed by the same customer at the same merchant. The " +
	"single most useful signal when judging whether a dispute is worth fighting: a " +
	"first-time claim reads very differently from a fifth."

type CustomerHistoryInput struct {
	Merchant    string `json:"merchant" jsonschema:"Merchant external id, for example mrc_northwind"`
	CustomerRef string `json:"customer_ref" jsonschema:"Customer reference from get_dispute"`
	Limit       int    `json:"limit,omitempty" jsonschema:"How many prior disputes, 1 to 50. Defaults to 20"`
}

type CustomerHistoryOutput struct {
	Disputes []api.CustomerHistoryRow `json:"disputes"`
	Count    int                      `json:"count"`
}

func (s *Set) CustomerHistory(ctx context.Context, in CustomerHistoryInput) (CustomerHistoryOutput, error) {
	rows, err := s.store.CustomerHistory(ctx, in.Merchant, in.CustomerRef, clampLimit(in.Limit))
	if err != nil {
		return CustomerHistoryOutput{}, err
	}
	return CustomerHistoryOutput{Disputes: rows, Count: len(rows)}, nil
}

// ---------------------------------------------------------------------------
// queue_summary
// ---------------------------------------------------------------------------

const DescQueueSummary = "Counts by state and kind, money currently at risk per currency, and how " +
	"the open deadlines are distributed. Use this for questions about the queue as a " +
	"whole rather than about any one dispute."

type QueueSummaryInput struct {
	Merchant string `json:"merchant,omitempty" jsonschema:"Optional merchant external id to narrow the summary"`
}

type QueueSummaryOutput struct {
	ByState   []api.StateCount     `json:"by_state"`
	AtRisk    []api.Exposure       `json:"money_at_risk"`
	Deadlines []api.DeadlineBucket `json:"deadline_buckets"`
}

func (s *Set) QueueSummary(ctx context.Context, in QueueSummaryInput) (QueueSummaryOutput, error) {
	summary, err := s.store.Summary(ctx, api.Filters{Merchants: splitCSV(in.Merchant)})
	if err != nil {
		return QueueSummaryOutput{}, err
	}
	return QueueSummaryOutput{
		ByState: summary.States, AtRisk: summary.OpenValue, Deadlines: summary.Deadlines,
	}, nil
}
