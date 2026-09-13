package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// A Filters literal with nothing set has to render SQL Postgres accepts. It
// used to render "ORDER BY  DESC" and "LIMIT 0", which only parseFilters
// callers were spared.
func TestZeroFiltersRenderUsableSQL(t *testing.T) {
	t.Parallel()

	f := Filters{}.normalize()
	if f.Sort != defaultSort {
		t.Errorf("Sort = %q, want %q", f.Sort, defaultSort)
	}
	if f.Limit != defaultLimit {
		t.Errorf("Limit = %d, want %d", f.Limit, defaultLimit)
	}
	if order := f.orderBy(); !strings.Contains(order, sortColumns[defaultSort]) || strings.Contains(order, "ORDER BY  ") {
		t.Errorf("orderBy() = %q", order)
	}
}

func TestNormalizeKeepsWhatIsSetAndClampsTheRest(t *testing.T) {
	t.Parallel()

	for name, tc := range map[string]struct {
		in   Filters
		want Filters
	}{
		"explicit values survive": {
			in:   Filters{Sort: "deadline_at", Limit: 20, Offset: 40},
			want: Filters{Sort: "deadline_at", Limit: 20, Offset: 40},
		},
		"an unknown sort falls back": {
			in:   Filters{Sort: "nope", Limit: 20},
			want: Filters{Sort: defaultSort, Limit: 20},
		},
		"a limit above the cap is clamped": {
			in:   Filters{Sort: defaultSort, Limit: maxLimit + 1},
			want: Filters{Sort: defaultSort, Limit: maxLimit},
		},
		"a negative offset is zero": {
			in:   Filters{Sort: defaultSort, Limit: 1, Offset: -5},
			want: Filters{Sort: defaultSort, Limit: 1},
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			got := tc.in.normalize()
			if got.Sort != tc.want.Sort || got.Limit != tc.want.Limit || got.Offset != tc.want.Offset {
				t.Errorf("normalize() = {Sort:%q Limit:%d Offset:%d}, want {Sort:%q Limit:%d Offset:%d}",
					got.Sort, got.Limit, got.Offset, tc.want.Sort, tc.want.Limit, tc.want.Offset)
			}
		})
	}
}

// parseFilters is the whole boundary between a query string and the SQL this
// package builds, and it draws one line twice over: a value that would change
// which rows come back is refused, while a value that only changes how they are
// presented falls back. Getting that backwards shows an operator more data than
// they asked to see and tells them it is all there is.
func TestParseFiltersRefusesWhatWouldChangeTheRows(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name    string
		query   string
		wantErr bool
		check   func(t *testing.T, f Filters)
	}{
		{
			name:  "an empty query string is the defaults",
			query: "",
			check: func(t *testing.T, f Filters) {
				if f.Sort != defaultSort || f.Limit != defaultLimit || f.Offset != 0 {
					t.Errorf("sort=%q limit=%d offset=%d, want %q/%d/0",
						f.Sort, f.Limit, f.Offset, defaultSort, defaultLimit)
				}
				if !f.Desc {
					t.Error("the default direction is not descending; the newest disputes are on the last page")
				}
			},
		},

		// The whitelists. An unknown value here would return an empty page that
		// reads as "no disputes match" rather than "you asked for a state that
		// cannot exist".
		{
			name:  "every state the schema allows is accepted",
			query: "state=received,resolving,draft_ready,refunded,represented,won,lost,expired",
			check: func(t *testing.T, f Filters) {
				if len(f.States) != 8 {
					t.Errorf("states = %v; a state the database can hold was dropped", f.States)
				}
			},
		},
		{name: "a state the schema has no room for is refused", query: "state=pending", wantErr: true},
		{
			name:  "both kinds are accepted",
			query: "kind=alert,chargeback",
			check: func(t *testing.T, f Filters) {
				if len(f.Kinds) != 2 {
					t.Errorf("kinds = %v", f.Kinds)
				}
			},
		},
		{name: "a kind that is not one of the two is refused", query: "kind=retrieval", wantErr: true},
		{
			name:  "the four card networks are accepted",
			query: "network=visa,mastercard,amex,discover",
			check: func(t *testing.T, f Filters) {
				if len(f.Networks) != 4 {
					t.Errorf("networks = %v", f.Networks)
				}
			},
		},
		{name: "a network nobody issues on is refused", query: "network=maestro", wantErr: true},
		{name: "a sort key with no column behind it is refused", query: "sort=letter", wantErr: true},
		{name: "a currency that is not a 3-letter code is refused", query: "currency=dollars", wantErr: true},

		// The limit. Absent is the default; present but unusable is refused
		// rather than quietly replaced, because a caller who asked for a page
		// size and got a different one has no way to tell.
		{
			name:  "a limit above the cap is clamped to the cap, not reset to the default",
			query: "limit=201",
			check: func(t *testing.T, f Filters) {
				if f.Limit != maxLimit {
					t.Errorf("limit = %d, want %d: asking for 201 must not return fewer rows than asking for 200", f.Limit, maxLimit)
				}
			},
		},
		{
			name:  "a limit under the cap is taken as asked",
			query: "limit=7",
			check: func(t *testing.T, f Filters) {
				if f.Limit != 7 {
					t.Errorf("limit = %d, want 7", f.Limit)
				}
			},
		},
		{name: "a limit of zero is refused", query: "limit=0", wantErr: true},
		{name: "a limit that is not a number is refused", query: "limit=all", wantErr: true},

		{name: "a negative offset is refused", query: "offset=-1", wantErr: true},
		{name: "an offset past the deep-paging cap is refused", query: "offset=10001", wantErr: true},
		{
			name:  "an offset inside the cap is taken as asked",
			query: "offset=10000",
			check: func(t *testing.T, f Filters) {
				if f.Offset != maxOffset {
					t.Errorf("offset = %d, want %d", f.Offset, maxOffset)
				}
			},
		},

		{name: "a date that is not RFC3339 is refused", query: "opened_from=2026-09-13", wantErr: true},
		{
			name:  "an RFC3339 date is parsed",
			query: "opened_from=2026-09-13T00:00:00Z",
			check: func(t *testing.T, f Filters) {
				if f.OpenedFrom == nil || f.OpenedFrom.Year() != 2026 {
					t.Errorf("opened_from = %v", f.OpenedFrom)
				}
			},
		},
		{name: "an amount with a decimal point in it is refused", query: "min_amount=49.99", wantErr: true},
		{name: "a negative amount is refused", query: "min_amount=-1", wantErr: true},
		{name: "a duration that is not one is refused", query: "due_within=soon", wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			f, err := parseFilters(httptest.NewRequest(http.MethodGet, "/api/disputes?"+tc.query, nil))
			if tc.wantErr {
				if err == nil {
					t.Fatalf("?%s was accepted as %+v; a filter that cannot match was ignored", tc.query, f)
				}
				return
			}
			if err != nil {
				t.Fatalf("?%s was refused: %v", tc.query, err)
			}
			if tc.check != nil {
				tc.check(t, f)
			}
		})
	}
}
