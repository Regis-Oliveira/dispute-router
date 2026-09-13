package api

import (
	"strings"
	"testing"
)

// A Filters literal with nothing set has to render SQL Postgres accepts. It
// used to render "ORDER BY  DESC" and "LIMIT 0", which only ParseFilters
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
