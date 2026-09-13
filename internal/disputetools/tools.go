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
	"unicode/utf8"

	"github.com/regisoliveira/dispute-router/internal/api"
	"github.com/regisoliveira/dispute-router/internal/money"
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

// New binds the tools to a read model.
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
	// The first rune, not the first byte: slicing "é" at [:1] leaves half a
	// character, which is invalid UTF-8 in a JSON tool result.
	first, width := utf8.DecodeRuneInString(local)
	if width < len(local) {
		local = string(first) + strings.Repeat("*", 3)
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
	"carry a negative hours_to_deadline. " + descAmounts

// descAmounts is appended to every tool that returns money. A model told to
// copy figures from a record copies amount_minor, and a letter or an answer
// that says 5799 about a $57.99 dispute is off by a factor of a hundred. The
// formatted field is the one to quote; the integer exists for arithmetic.
const descAmounts = "Amounts appear twice: `amount` is the figure to quote, already formatted with " +
	"its currency (for example 57.99 USD or 5,000 JPY); `amount_minor` is the same value " +
	"as an integer in the currency's smallest unit, for arithmetic only. Never state " +
	"amount_minor as an amount."

// ListDisputesInput is the list_disputes arguments.
type ListDisputesInput struct {
	State     string `json:"state,omitempty" jsonschema:"Comma-separated states: received, resolving, represented, refunded, won, lost, expired"`
	Kind      string `json:"kind,omitempty" jsonschema:"alert or chargeback. Alerts are pre-dispute warnings with hours to act; chargebacks are already filed"`
	Merchant  string `json:"merchant,omitempty" jsonschema:"Merchant external id, for example mrc_northwind"`
	DueWithin string `json:"due_within,omitempty" jsonschema:"Only open disputes whose deadline falls inside this window, for example 24h. Already-overdue disputes match too and are flagged with overdue=true"`
	OpenOnly  bool   `json:"open_only,omitempty" jsonschema:"Only disputes still awaiting a decision"`
	Limit     int    `json:"limit,omitempty" jsonschema:"How many to return, 1 to 50. Defaults to 20"`
}

// DisputeSummary is one list_disputes row.
type DisputeSummary struct {
	ID        int64  `json:"id"`
	Reference string `json:"reference"`
	// Merchant is the external id, because it is what every tool that takes a
	// merchant expects. Returning only the display name here meant a model
	// could read "Northwind Supply" out of one tool and pass it to another that
	// matches on m.external_id, which quietly answers with nothing at all.
	Merchant        string `json:"merchant"`
	MerchantName    string `json:"merchant_name"`
	Kind            string `json:"kind"`
	State           string `json:"state"`
	ReasonCode      string `json:"reason_code"`
	Amount          string `json:"amount"`
	AmountMinor     int64  `json:"amount_minor"`
	Currency        string `json:"currency"`
	HoursToDeadline int    `json:"hours_to_deadline"`
	// Overdue is stated rather than left as the sign of the number above. A
	// negative hours_to_deadline is easy to skim past; "overdue": true is not,
	// and the difference is whether the reader thinks there is time left.
	Overdue  bool   `json:"overdue"`
	OpenedAt string `json:"opened_at"`
}

// ListDisputesOutput is the list_disputes answer.
type ListDisputesOutput struct {
	Disputes []DisputeSummary `json:"disputes"`
	Total    int64            `json:"total_matching"`
	Note     string           `json:"note,omitempty"`
}

// ListDisputes is the list_disputes tool.
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
			Merchant:        row.MerchantID,
			MerchantName:    row.MerchantName,
			Kind:            row.Kind,
			State:           row.State,
			ReasonCode:      row.ReasonCode,
			Amount:          money.FormatMinor(row.Amount.AmountMinor, row.Amount.Currency),
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

// DescGetDispute is the get_dispute prompt.
const DescGetDispute = "Everything known about one dispute: the amounts, the original charge, " +
	"the full state history with who caused each transition, and the ledger entries it " +
	"produced. Use this before reasoning about a specific dispute - the list view " +
	"deliberately omits most of it. " + descAmounts

// GetDisputeInput is the get_dispute arguments.
type GetDisputeInput struct {
	ID int64 `json:"id" jsonschema:"The dispute's numeric id, from list_disputes"`
}

// LedgerLine is one ledger posting as get_dispute reports it.
type LedgerLine struct {
	Entry       string `json:"entry"`
	Account     string `json:"account"`
	Direction   string `json:"direction"`
	Amount      string `json:"amount"`
	AmountMinor int64  `json:"amount_minor"`
}

// HistoryLine is one state transition as get_dispute reports it.
type HistoryLine struct {
	At    string `json:"at"`
	From  string `json:"from,omitempty"`
	To    string `json:"to"`
	Actor string `json:"actor"`
}

// GetDisputeOutput is the get_dispute answer.
type GetDisputeOutput struct {
	ID           int64  `json:"id"`
	Reference    string `json:"reference"`
	Merchant     string `json:"merchant"`
	MerchantName string `json:"merchant_name"`
	Kind         string `json:"kind"`
	State        string `json:"state"`
	ReasonCode   string `json:"reason_code"`
	CardNetwork  string `json:"card_network"`
	// The formatted strings are the figures to quote; the integers beside
	// them are for arithmetic. Both are here because a tool answer is read by
	// a model, and a model handed only the integer writes it as the amount.
	Amount             string        `json:"amount"`
	AmountMinor        int64         `json:"amount_minor"`
	Currency           string        `json:"currency"`
	OriginalChargeText string        `json:"original_charge"`
	OriginalCharge     int64         `json:"original_charge_minor"`
	RefundedText       string        `json:"refunded,omitempty"`
	RefundedMinor      int64         `json:"refunded_minor"`
	HoursToDeadline    int           `json:"hours_to_deadline"`
	Overdue            bool          `json:"overdue"`
	OpenedAt           string        `json:"opened_at"`
	ChargedAt          string        `json:"charged_at"`
	Descriptor         string        `json:"statement_descriptor"`
	CardLast4          string        `json:"card_last4"`
	CustomerRef        string        `json:"customer_ref"`
	CustomerEmail      string        `json:"customer_email_masked"`
	History            []HistoryLine `json:"history"`
	Ledger             []LedgerLine  `json:"ledger"`
}

// GetDispute is the tool. It never returns the cardholder's claim.
//
// The claim is free text written by the party trying to reverse the charge, and
// this output goes to an MCP client - a transcript with no way to mark a span
// as untrusted, read by a model that was told everything here is the record.
// Callers that can quarantine it ask for it explicitly.
func (s *Set) GetDispute(ctx context.Context, in GetDisputeInput) (GetDisputeOutput, error) {
	out, _, err := s.DisputeWithClaim(ctx, in)
	return out, err
}

// DisputeWithClaim returns the same record plus the cardholder's own words,
// separately rather than as a field, so that a caller cannot pass it on without
// having decided what to do with it.
func (s *Set) DisputeWithClaim(ctx context.Context, in GetDisputeInput) (GetDisputeOutput, string, error) {
	d, err := s.store.Dispute(ctx, in.ID)
	if err != nil {
		return GetDisputeOutput{}, "", err
	}

	out := GetDisputeOutput{
		ID: d.ID, Reference: d.ExternalID, Merchant: d.MerchantID, MerchantName: d.MerchantName,
		Kind: d.Kind, State: d.State, ReasonCode: d.ReasonCode, CardNetwork: d.CardNetwork,
		Amount:             money.FormatMinor(d.Amount.AmountMinor, d.Amount.Currency),
		AmountMinor:        d.Amount.AmountMinor,
		Currency:           d.Amount.Currency,
		OriginalChargeText: money.FormatMinor(d.OriginalAmount.AmountMinor, d.OriginalAmount.Currency),
		OriginalCharge:     d.OriginalAmount.AmountMinor,
		RefundedMinor:      d.RefundedMinor,
		HoursToDeadline:    int(d.SecondsToDeadline / 3600),
		Overdue:            d.SecondsToDeadline < 0 && d.ResolvedAt == nil,
		OpenedAt:           d.OpenedAt.UTC().Format(time.RFC3339),
		ChargedAt:          d.CapturedAt.UTC().Format(time.RFC3339),
		Descriptor:         d.Descriptor,
		CardLast4:          d.CardLast4,
		CustomerRef:        d.CustomerRef,
		CustomerEmail:      maskEmail(d.CustomerEmail),
	}

	if d.RefundedMinor > 0 {
		out.RefundedText = money.FormatMinor(d.RefundedMinor, d.Amount.Currency)
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
			Amount:      money.FormatMinor(p.Amount.AmountMinor, p.Amount.Currency),
			AmountMinor: p.Amount.AmountMinor,
		})
	}
	return out, d.CardholderClaim, nil
}

// ---------------------------------------------------------------------------
// get_customer_history
// ---------------------------------------------------------------------------

// DescCustomerHistory is the get_customer_history prompt.
const DescCustomerHistory = "Every dispute filed by the same customer at the same merchant, the one " +
	"being asked about included. The single most useful signal when judging whether a " +
	"dispute is worth fighting: a first-time claim reads very differently from a fifth. " +
	"Pass the merchant field from list_disputes or get_dispute, not the merchant_name. " +
	descAmounts

// CustomerHistoryEntry is one dispute by the same customer, in the shape a
// model reads: a formatted amount beside the integer, and a timestamp in the
// same form every other tool uses.
type CustomerHistoryEntry struct {
	ID          int64  `json:"id"`
	Reference   string `json:"reference"`
	Kind        string `json:"kind"`
	State       string `json:"state"`
	ReasonCode  string `json:"reason_code"`
	Amount      string `json:"amount"`
	AmountMinor int64  `json:"amount_minor"`
	Currency    string `json:"currency"`
	OpenedAt    string `json:"opened_at"`
}

// CustomerHistoryInput is the get_customer_history arguments.
type CustomerHistoryInput struct {
	Merchant    string `json:"merchant" jsonschema:"Merchant external id, for example mrc_northwind"`
	CustomerRef string `json:"customer_ref" jsonschema:"Customer reference from get_dispute"`
	Limit       int    `json:"limit,omitempty" jsonschema:"How many prior disputes, 1 to 50. Defaults to 20"`
}

// CustomerHistoryOutput is the get_customer_history answer.
type CustomerHistoryOutput struct {
	Disputes []CustomerHistoryEntry `json:"disputes"`
	Count    int                    `json:"count"`
}

// CustomerHistory is the get_customer_history tool.
func (s *Set) CustomerHistory(ctx context.Context, in CustomerHistoryInput) (CustomerHistoryOutput, error) {
	rows, err := s.store.CustomerHistory(ctx, in.Merchant, in.CustomerRef, clampLimit(in.Limit))
	if err != nil {
		return CustomerHistoryOutput{}, err
	}
	out := CustomerHistoryOutput{Disputes: make([]CustomerHistoryEntry, 0, len(rows)), Count: len(rows)}
	for _, row := range rows {
		out.Disputes = append(out.Disputes, CustomerHistoryEntry{
			ID: row.ID, Reference: row.ExternalID, Kind: row.Kind, State: row.State,
			ReasonCode:  row.ReasonCode,
			Amount:      money.FormatMinor(row.Amount.AmountMinor, row.Amount.Currency),
			AmountMinor: row.Amount.AmountMinor,
			Currency:    row.Amount.Currency,
			OpenedAt:    row.OpenedAt.UTC().Format(time.RFC3339),
		})
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// queue_summary
// ---------------------------------------------------------------------------

// DescQueueSummary is the queue_summary prompt.
const DescQueueSummary = "Counts by state and kind, money currently at risk per currency, and how " +
	"the open deadlines are distributed. Use this for questions about the queue as a " +
	"whole rather than about any one dispute."

// QueueSummaryInput is the queue_summary arguments.
type QueueSummaryInput struct {
	Merchant string `json:"merchant,omitempty" jsonschema:"Optional merchant external id to narrow the summary"`
}

// QueueSummaryOutput is the queue_summary answer.
type QueueSummaryOutput struct {
	ByState   []api.StateCount     `json:"by_state"`
	AtRisk    []api.Exposure       `json:"money_at_risk"`
	Deadlines []api.DeadlineBucket `json:"deadline_buckets"`
}

// QueueSummary is the queue_summary tool.
func (s *Set) QueueSummary(ctx context.Context, in QueueSummaryInput) (QueueSummaryOutput, error) {
	summary, err := s.store.Summary(ctx, api.Filters{Merchants: splitCSV(in.Merchant)})
	if err != nil {
		return QueueSummaryOutput{}, err
	}
	return QueueSummaryOutput{
		ByState: summary.States, AtRisk: summary.OpenValue, Deadlines: summary.Deadlines,
	}, nil
}
