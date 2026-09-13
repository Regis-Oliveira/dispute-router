package api

import (
	"context"
	"fmt"
	"time"

	"golang.org/x/sync/errgroup"
)

// StateCount is how many disputes sit in one state and kind.
type StateCount struct {
	State string `json:"state"`
	Kind  string `json:"kind"`
	Count int64  `json:"count"`
}

// Exposure is the open money in one currency.
type Exposure struct {
	Currency    string `json:"currency"`
	AmountMinor int64  `json:"amount_minor"`
	Count       int64  `json:"count"`
}

// DeadlineBucket answers the only question an operator opens this page to ask:
// what runs out first.
type DeadlineBucket struct {
	Bucket string `json:"bucket"`
	Count  int64  `json:"count"`
}

// DailyPoint is one day's arrivals, split by kind.
type DailyPoint struct {
	Day         string `json:"day"`
	Alerts      int64  `json:"alerts"`
	Chargebacks int64  `json:"chargebacks"`
}

// Summary is what the overview page shows.
type Summary struct {
	States    []StateCount     `json:"states"`
	OpenValue []Exposure       `json:"open_value"`
	Deadlines []DeadlineBucket `json:"deadlines"`
	Daily     []DailyPoint     `json:"daily"`
	TookMS    int64            `json:"took_ms"`
}

// Summary runs its four independent queries concurrently. Serially they are
// four round trips the operator waits through in sequence; the slowest one
// alone is the honest cost of this page.
func (s *Store) Summary(ctx context.Context, f Filters) (Summary, error) {
	started := time.Now()

	var b builder
	f.apply(&b, started.UTC())
	where := b.where()

	var out Summary
	group, groupCtx := errgroup.WithContext(ctx)

	group.Go(func() error {
		rows, err := s.pool.Query(groupCtx, `
			SELECT d.state, d.kind, count(*)
			  FROM disputes d
			  JOIN merchants m    ON m.id = d.merchant_id
			  JOIN transactions t ON t.id = d.transaction_id`+where+`
			 GROUP BY d.state, d.kind
			 ORDER BY count(*) DESC`, b.args...)
		if err != nil {
			return fmt.Errorf("summary states: %w", err)
		}
		defer rows.Close()

		out.States = []StateCount{}
		for rows.Next() {
			var c StateCount
			if err := rows.Scan(&c.State, &c.Kind, &c.Count); err != nil {
				return err
			}
			out.States = append(out.States, c)
		}
		return rows.Err()
	})

	group.Go(func() error {
		// Money at risk right now: everything still on the clock. Grouped by
		// currency because summing across currencies is not a number.
		rows, err := s.pool.Query(groupCtx, `
			SELECT d.currency, SUM(d.amount_minor), count(*)
			  FROM disputes d
			  JOIN merchants m    ON m.id = d.merchant_id
			  JOIN transactions t ON t.id = d.transaction_id`+where+
			openClause(where)+`
			 GROUP BY d.currency
			 ORDER BY SUM(d.amount_minor) DESC`, b.args...)
		if err != nil {
			return fmt.Errorf("summary exposure: %w", err)
		}
		defer rows.Close()

		out.OpenValue = []Exposure{}
		for rows.Next() {
			var e Exposure
			if err := rows.Scan(&e.Currency, &e.AmountMinor, &e.Count); err != nil {
				return err
			}
			out.OpenValue = append(out.OpenValue, e)
		}
		return rows.Err()
	})

	group.Go(func() error {
		rows, err := s.pool.Query(groupCtx, `
			SELECT bucket, count(*) FROM (
			  SELECT CASE
			           WHEN d.deadline_at <= now() THEN 'overdue'
			           WHEN d.deadline_at <= now() + INTERVAL '6 hours'  THEN '6h'
			           WHEN d.deadline_at <= now() + INTERVAL '24 hours' THEN '24h'
			           WHEN d.deadline_at <= now() + INTERVAL '72 hours' THEN '72h'
			           ELSE 'later'
			         END AS bucket
			    FROM disputes d
			    JOIN merchants m    ON m.id = d.merchant_id
			    JOIN transactions t ON t.id = d.transaction_id`+where+
			openClause(where)+`
			) buckets
			 GROUP BY bucket`, b.args...)
		if err != nil {
			return fmt.Errorf("summary deadlines: %w", err)
		}
		defer rows.Close()

		found := map[string]int64{}
		for rows.Next() {
			var bucket string
			var count int64
			if err := rows.Scan(&bucket, &count); err != nil {
				return err
			}
			found[bucket] = count
		}
		if err := rows.Err(); err != nil {
			return err
		}

		// Emit every bucket, including the empty ones. A chart whose bars
		// appear and disappear between refreshes cannot be read at a glance.
		out.Deadlines = make([]DeadlineBucket, 0, 5)
		for _, name := range []string{"overdue", "6h", "24h", "72h", "later"} {
			out.Deadlines = append(out.Deadlines, DeadlineBucket{Bucket: name, Count: found[name]})
		}
		return nil
	})

	group.Go(func() error {
		// generate_series gives every day in the window a row, so a quiet day
		// is a zero rather than a gap the chart would draw straight through.
		//
		// The two kinds stay literal. Binding them would leave the column
		// aliases - alerts, chargebacks, and the JSON field names behind them -
		// spelling the same vocabulary out anyway, so it would move the
		// duplication rather than remove it.
		rows, err := s.pool.Query(groupCtx, `
			WITH days AS (
			  SELECT generate_series(
			    date_trunc('day', now()) - INTERVAL '29 days',
			    date_trunc('day', now()),
			    INTERVAL '1 day'
			  )::date AS day
			),
			counted AS (
			  SELECT date_trunc('day', d.opened_at)::date AS day,
			         count(*) FILTER (WHERE d.kind = 'alert')      AS alerts,
			         count(*) FILTER (WHERE d.kind = 'chargeback') AS chargebacks
			    FROM disputes d
			    JOIN merchants m    ON m.id = d.merchant_id
			    JOIN transactions t ON t.id = d.transaction_id`+where+`
			   GROUP BY 1
			)
			SELECT days.day, COALESCE(counted.alerts, 0), COALESCE(counted.chargebacks, 0)
			  FROM days LEFT JOIN counted ON counted.day = days.day
			 ORDER BY days.day`, b.args...)
		if err != nil {
			return fmt.Errorf("summary daily: %w", err)
		}
		defer rows.Close()

		out.Daily = []DailyPoint{}
		for rows.Next() {
			var day time.Time
			var point DailyPoint
			if err := rows.Scan(&day, &point.Alerts, &point.Chargebacks); err != nil {
				return err
			}
			point.Day = day.Format(time.DateOnly)
			out.Daily = append(out.Daily, point)
		}
		return rows.Err()
	})

	if err := group.Wait(); err != nil {
		return Summary{}, err
	}

	out.TookMS = time.Since(started).Milliseconds()
	return out, nil
}

// openClause narrows to disputes still on the clock, whether or not the caller
// already supplied a WHERE.
//
// Spelled out rather than bound from internal/dispute for the same reason as
// Filters.apply: disputes_open_deadline_idx is partial on these two literals,
// and the planner cannot prove a bind parameter implies its predicate.
func openClause(where string) string {
	if where == "" {
		return " WHERE d.state IN ('received','resolving')"
	}
	return " AND d.state IN ('received','resolving')"
}

// MerchantOption is one entry in the merchant filter control.
type MerchantOption struct {
	ExternalID string `json:"external_id"`
	Name       string `json:"name"`
	Currency   string `json:"currency"`
	Disputes   int64  `json:"disputes"`
}

// Merchants populates the filter control. Sorted by volume, because a filter
// list in insertion order makes the operator hunt.
func (s *Store) Merchants(ctx context.Context) ([]MerchantOption, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT m.external_id, m.name, m.currency, count(d.id)
		  FROM merchants m
		  LEFT JOIN disputes d ON d.merchant_id = m.id
		 GROUP BY m.id, m.external_id, m.name, m.currency
		 ORDER BY count(d.id) DESC, m.name`)
	if err != nil {
		return nil, fmt.Errorf("list merchants: %w", err)
	}
	defer rows.Close()

	out := []MerchantOption{}
	for rows.Next() {
		var m MerchantOption
		if err := rows.Scan(&m.ExternalID, &m.Name, &m.Currency, &m.Disputes); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}
