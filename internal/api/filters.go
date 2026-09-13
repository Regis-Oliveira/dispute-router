package api

import (
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/regisoliveira/dispute-router/internal/dispute"
)

// builder accumulates WHERE fragments and their arguments together, so a
// condition and the value it tests can never drift apart.
//
// Every value reaches Postgres as a bind parameter. No caller-supplied string
// is ever concatenated into SQL - not a filter value, and not a column name
// either: sort and direction come from the whitelists below, so a request can
// only ever select from expressions written here.
type builder struct {
	conds []string
	args  []any
}

func (b *builder) add(fragment string, value any) {
	b.conds = append(b.conds, b.expr(fragment, value))
}

// expr binds values and returns the fragment that reads them, without making it
// a condition. It is what add is built from, and what a caller uses when the
// same expression belongs somewhere the WHERE clause is not: decisions.go
// computes the override pair in its SELECT list as well.
func (b *builder) expr(fragment string, values ...any) string {
	positions := make([]any, len(values))
	for i, value := range values {
		b.args = append(b.args, value)
		positions[i] = len(b.args)
	}
	return fmt.Sprintf(fragment, positions...)
}

func (b *builder) where() string {
	if len(b.conds) == 0 {
		return ""
	}
	return " WHERE " + strings.Join(b.conds, " AND ")
}

// sortColumns maps the API's sort keys to real SQL. A key that is not in this
// map is rejected, which is what makes "?sort=" safe to expose at all.
var sortColumns = map[string]string{
	"opened_at":   "d.opened_at",
	"deadline_at": "d.deadline_at",
	"amount":      "d.amount_minor",
	"state":       "d.state",
	"merchant":    "m.name",
	"kind":        "d.kind",
}

const defaultSort = "opened_at"

// Filters is one dashboard query.
type Filters struct {
	Merchants []string
	States    []string
	Kinds     []string
	Networks  []string
	Reasons   []string
	Currency  string

	MinAmountMinor *int64
	MaxAmountMinor *int64

	OpenedFrom *time.Time
	OpenedTo   *time.Time

	// DueWithin narrows to open disputes whose deadline lands inside a window
	// from now - the "what do I have to act on today" question.
	DueWithin *time.Duration

	// OpenOnly is the states the deadline sweeper still owns.
	OpenOnly bool

	// Search matches a dispute or transaction reference.
	Search string

	Sort   string
	Desc   bool
	Limit  int
	Offset int
}

const (
	defaultLimit = 50
	maxLimit     = 200
	// maxOffset caps how deep a client can page. OFFSET makes Postgres walk and
	// discard every row before the window, so page 10,000 is a table scan
	// wearing a page number. Deep paging is a symptom of the wrong tool: that
	// user wants the CSV export, which streams instead.
	maxOffset = 10_000
)

// parseFilters reads a request's query string. Anything unparseable falls back
// to the default rather than 400-ing, except values that would change which
// rows are returned - those are refused, because silently ignoring a filter
// shows the operator more data than they asked to see.
func parseFilters(r *http.Request) (Filters, error) {
	q := r.URL.Query()

	f := Filters{
		Merchants: csvValues(q.Get("merchant")),
		States:    csvValues(q.Get("state")),
		Kinds:     csvValues(q.Get("kind")),
		Networks:  csvValues(q.Get("network")),
		Reasons:   csvValues(q.Get("reason")),
		Currency:  strings.ToUpper(strings.TrimSpace(q.Get("currency"))),
		Search:    strings.TrimSpace(q.Get("q")),
		OpenOnly:  q.Get("open") == "true",
		Desc:      q.Get("dir") != "asc",
		Sort:      defaultSort,
	}

	if raw := q.Get("sort"); raw != "" {
		if _, ok := sortColumns[raw]; !ok {
			return Filters{}, fmt.Errorf("unknown sort %q", raw)
		}
		f.Sort = raw
	}

	limit, err := parseLimit(q)
	if err != nil {
		return Filters{}, err
	}
	f.Limit = limit

	offset, err := parseOffset(q)
	if err != nil {
		return Filters{}, err
	}
	f.Offset = offset

	for _, spec := range []struct {
		key    string
		target **int64
	}{
		{"min_amount", &f.MinAmountMinor},
		{"max_amount", &f.MaxAmountMinor},
	} {
		if raw := q.Get(spec.key); raw != "" {
			// Minor units, always. The API never accepts a decimal amount,
			// because "49.99" has to become an integer somewhere and the
			// boundary is the one place that conversion is visible.
			n, err := strconv.ParseInt(raw, 10, 64)
			if err != nil || n < 0 {
				return Filters{}, fmt.Errorf("%s must be a non-negative integer of minor units", spec.key)
			}
			*spec.target = &n
		}
	}

	for _, spec := range []struct {
		key    string
		target **time.Time
	}{
		{"opened_from", &f.OpenedFrom},
		{"opened_to", &f.OpenedTo},
	} {
		if raw := q.Get(spec.key); raw != "" {
			t, err := time.Parse(time.RFC3339, raw)
			if err != nil {
				return Filters{}, fmt.Errorf("%s must be an RFC3339 timestamp", spec.key)
			}
			*spec.target = &t
		}
	}

	if raw := q.Get("due_within"); raw != "" {
		d, err := time.ParseDuration(raw)
		if err != nil || d <= 0 {
			return Filters{}, fmt.Errorf("due_within must be a positive duration such as 24h")
		}
		f.DueWithin = &d
	}

	if err := f.validateEnums(); err != nil {
		return Filters{}, err
	}
	return f, nil
}

// parseLimit reads ?limit for every endpoint that pages, so that one query
// string means one thing across the API. Absent is the default page size;
// present but not a positive integer is refused rather than quietly replaced,
// because a caller who asked for a page size and got a different one has no
// way to tell.
//
// Above the cap is clamped to the cap, never reset to the default: ?limit=201
// must not return fewer rows than ?limit=200.
func parseLimit(q url.Values) (int, error) {
	raw := q.Get("limit")
	if raw == "" {
		return defaultLimit, nil
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < 1 {
		return 0, errors.New("limit must be a positive integer")
	}
	return min(n, maxLimit), nil
}

// parseOffset reads ?offset on the same terms as parseLimit.
func parseOffset(q url.Values) (int, error) {
	raw := q.Get("offset")
	if raw == "" {
		return 0, nil
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < 0 {
		return 0, errors.New("offset must be a non-negative integer")
	}
	if n > maxOffset {
		return 0, fmt.Errorf("offset above %d is not supported; narrow the filters or use the CSV export", maxOffset)
	}
	return n, nil
}

var (
	// Every state the schema allows, so that a filter can name any row the
	// database can hold. draft_ready was added by migration 000003 and for
	// five days reached none of the four places that list states by hand: this
	// whitelist, the dashboard's union and filter control, the schema tag the
	// agent reads, and the simulator's money invariant - which failed, because
	// a chargeback awaiting review still holds its funds.
	knownStates = stringSet(
		dispute.StateReceived, dispute.StateResolving, dispute.StateDraftReady,
		dispute.StateRefunded, dispute.StateRepresented, dispute.StateWon,
		dispute.StateLost, dispute.StateExpired,
	)
	knownKinds    = stringSet(dispute.KindAlert, dispute.KindChargeback)
	knownNetworks = stringSet("visa", "mastercard", "amex", "discover")
)

// normalize fills in what a caller left at zero so that the value renders
// usable SQL. parseFilters sets every default itself; this exists for the
// callers that build a Filters literal (disputetools, tests), where a missing
// Sort used to render "ORDER BY  DESC" and a missing Limit "LIMIT 0".
func (f Filters) normalize() Filters {
	if _, ok := sortColumns[f.Sort]; !ok {
		f.Sort = defaultSort
	}
	if f.Limit <= 0 {
		f.Limit = defaultLimit
	}
	f.Limit = min(f.Limit, maxLimit)
	f.Offset = max(f.Offset, 0)
	return f
}

// validateEnums refuses a value the schema could never hold. Passing it through
// would return an empty page that looks like "no disputes match" rather than
// "you asked for something that does not exist".
func (f Filters) validateEnums() error {
	for _, check := range []struct {
		name   string
		values []string
		known  map[string]bool
	}{
		{"state", f.States, knownStates},
		{"kind", f.Kinds, knownKinds},
		{"network", f.Networks, knownNetworks},
	} {
		for _, value := range check.values {
			if !check.known[value] {
				return fmt.Errorf("unknown %s %q", check.name, value)
			}
		}
	}
	if f.Currency != "" && len(f.Currency) != 3 {
		return fmt.Errorf("currency must be a 3-letter code")
	}
	return nil
}

// apply turns the filters into WHERE conditions.
func (f Filters) apply(b *builder, now time.Time) {
	if len(f.Merchants) > 0 {
		b.add("m.external_id = ANY($%d)", f.Merchants)
	}
	if len(f.States) > 0 {
		b.add("d.state = ANY($%d)", f.States)
	}
	if len(f.Kinds) > 0 {
		b.add("d.kind = ANY($%d)", f.Kinds)
	}
	if len(f.Networks) > 0 {
		b.add("d.card_network = ANY($%d)", f.Networks)
	}
	if len(f.Reasons) > 0 {
		b.add("d.reason_code = ANY($%d)", f.Reasons)
	}
	if f.Currency != "" {
		b.add("d.currency = $%d", f.Currency)
	}
	if f.MinAmountMinor != nil {
		b.add("d.amount_minor >= $%d", *f.MinAmountMinor)
	}
	if f.MaxAmountMinor != nil {
		b.add("d.amount_minor <= $%d", *f.MaxAmountMinor)
	}
	if f.OpenedFrom != nil {
		b.add("d.opened_at >= $%d", *f.OpenedFrom)
	}
	if f.OpenedTo != nil {
		b.add("d.opened_at < $%d", *f.OpenedTo)
	}
	if f.OpenOnly || f.DueWithin != nil {
		// Matches the partial index on (deadline_at) WHERE state IN (...), so
		// this filter reads from a small index instead of the whole table. The
		// literals are the reason it does: a bind parameter is not something
		// the planner can prove implies the index's predicate, so binding
		// dispute.StateReceived here would cost the index.
		b.conds = append(b.conds, "d.state IN ('received','resolving')")
	}
	if f.DueWithin != nil {
		b.add("d.deadline_at <= $%d", now.Add(*f.DueWithin))
	}
	if f.Search != "" {
		// One argument, referenced by two placeholders - so it is bound once
		// and compared against both reference columns. Anchored (no leading
		// wildcard) so the index is usable.
		b.args = append(b.args, f.Search)
		n := len(b.args)
		b.conds = append(b.conds, fmt.Sprintf(
			"(d.external_id LIKE $%d || '%%' OR t.external_id LIKE $%d || '%%')", n, n))
	}
}

func (f Filters) orderBy() string {
	direction := "ASC"
	if f.Desc {
		direction = "DESC"
	}
	// d.id breaks ties so paging is stable: without it two rows with the same
	// opened_at can swap places between page 1 and page 2, and a row is shown
	// twice while another is never shown at all.
	return fmt.Sprintf(" ORDER BY %s %s, d.id %s", sortColumns[f.Sort], direction, direction)
}

func csvValues(raw string) []string {
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

// stringSet collects a typed vocabulary into a set keyed by the raw string,
// because what it is tested against is a value straight off the query string.
func stringSet[T ~string](values ...T) map[string]bool {
	m := make(map[string]bool, len(values))
	for _, v := range values {
		m[string(v)] = true
	}
	return m
}
