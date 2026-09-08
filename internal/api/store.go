package api

import (
	"context"
	"encoding/csv"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"golang.org/x/sync/errgroup"
)

var (
	ErrNotFound = errors.New("not found")
	// ErrInvalidInput is anything the caller could fix, and always answers 400.
	ErrInvalidInput = errors.New("invalid input")
)

type Store struct{ pool *pgxpool.Pool }

func NewStore(pool *pgxpool.Pool) *Store { return &Store{pool: pool} }

func (s *Store) Ping(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	return s.pool.Ping(ctx)
}

// Money travels as an integer count of minor units plus its currency, never a
// formatted string and never a float. Formatting is a presentation decision and
// belongs to whichever client is doing the presenting.
type Money struct {
	AmountMinor int64  `json:"amount_minor"`
	Currency    string `json:"currency"`
}

type DisputeRow struct {
	ID           int64      `json:"id"`
	ExternalID   string     `json:"external_id"`
	MerchantID   string     `json:"merchant_id"`
	MerchantName string     `json:"merchant_name"`
	Kind         string     `json:"kind"`
	State        string     `json:"state"`
	CardNetwork  string     `json:"card_network"`
	ReasonCode   string     `json:"reason_code"`
	Amount       Money      `json:"amount"`
	OpenedAt     time.Time  `json:"opened_at"`
	DeadlineAt   time.Time  `json:"deadline_at"`
	ResolvedAt   *time.Time `json:"resolved_at"`

	TransactionRef string `json:"transaction_ref"`
	CardLast4      string `json:"card_last4"`
	CustomerRef    string `json:"customer_ref"`

	// SecondsToDeadline is negative once the deadline has passed. Computed by
	// Postgres against a single `now()` so every row in a page is measured from
	// the same instant - doing it per row in the client makes rows drift apart.
	SecondsToDeadline int64 `json:"seconds_to_deadline"`
}

type Page struct {
	Offset int   `json:"offset"`
	Limit  int   `json:"limit"`
	Total  int64 `json:"total"`
}

type DisputeList struct {
	Rows   []DisputeRow `json:"rows"`
	Page   Page         `json:"page"`
	TookMS int64        `json:"took_ms"`
}

const disputeColumns = `
	SELECT d.id, d.external_id, m.external_id, m.name,
	       d.kind, d.state, d.card_network, d.reason_code,
	       d.amount_minor, d.currency,
	       d.opened_at, d.deadline_at, d.resolved_at,
	       t.external_id, t.card_last4, t.customer_ref,
	       EXTRACT(EPOCH FROM (d.deadline_at - now()))::bigint`

// disputeSelect reads every display column and so joins everything. Used by the
// CSV export, which needs all of it for every row anyway.
const disputeSelect = disputeColumns + `
	  FROM disputes d
	  JOIN merchants m    ON m.id = d.merchant_id
	  JOIN transactions t ON t.id = d.transaction_id`

// scanSource is the FROM clause for a query that only has to *find* rows, not
// display them.
//
// merchants is eight rows and can be filtered and sorted on, so it always
// joins. transactions is half a million rows and is only ever a search target,
// so it joins only when there is a search: EXPLAIN showed the count query
// touching 39,358 buffers to probe a table whose columns nobody read, purely
// because the join was written once and reused everywhere.
func scanSource(f Filters) string {
	source := `
	  FROM disputes d
	  JOIN merchants m ON m.id = d.merchant_id`
	if f.Search != "" {
		source += `
	  JOIN transactions t ON t.id = d.transaction_id`
	}
	return source
}

func scanDispute(rows pgx.Rows) (DisputeRow, error) {
	var d DisputeRow
	err := rows.Scan(
		&d.ID, &d.ExternalID, &d.MerchantID, &d.MerchantName,
		&d.Kind, &d.State, &d.CardNetwork, &d.ReasonCode,
		&d.Amount.AmountMinor, &d.Amount.Currency,
		&d.OpenedAt, &d.DeadlineAt, &d.ResolvedAt,
		&d.TransactionRef, &d.CardLast4, &d.CustomerRef,
		&d.SecondsToDeadline,
	)
	return d, err
}

// ListDisputes runs the page query and its count concurrently.
//
// They are independent queries against the same snapshot-free connection pool,
// so waiting for one before starting the other doubles the latency of every
// dashboard request for no benefit.
func (s *Store) ListDisputes(ctx context.Context, f Filters) (DisputeList, error) {
	started := time.Now()
	now := started.UTC()

	var b builder
	f.apply(&b, now)
	where := b.where()

	var (
		list  []DisputeRow
		total int64
	)

	group, groupCtx := errgroup.WithContext(ctx)

	group.Go(func() error {
		args := append(append([]any{}, b.args...), f.Limit, f.Offset)

		// Two stages on purpose. The inner query finds the fifty ids for this
		// page and nothing else; only those fifty are then joined out to their
		// merchant and transaction for display.
		//
		// Written as one flat query instead, Postgres joins every matching
		// dispute to its transaction and *then* sorts and discards all but
		// fifty - 13,119 index probes to show 50 rows. The work should follow
		// the LIMIT, not precede it.
		sql := `
		WITH page AS (
			SELECT d.id` + scanSource(f) + where + f.orderBy() +
			fmt.Sprintf(" LIMIT $%d OFFSET $%d", len(b.args)+1, len(b.args)+2) + `
		)` + disputeColumns + `
		  FROM page
		  JOIN disputes d     ON d.id = page.id
		  JOIN merchants m    ON m.id = d.merchant_id
		  JOIN transactions t ON t.id = d.transaction_id` +
			// A CTE does not preserve order across the join, so the sort is
			// restated here. Dropping it is the classic version of this bug:
			// correct rows, arbitrary order, and it only shows up in review.
			f.orderBy()

		rows, err := s.pool.Query(groupCtx, sql, args...)
		if err != nil {
			return fmt.Errorf("list disputes: %w", err)
		}
		defer rows.Close()

		for rows.Next() {
			row, err := scanDispute(rows)
			if err != nil {
				return fmt.Errorf("scan dispute: %w", err)
			}
			list = append(list, row)
		}
		return rows.Err()
	})

	group.Go(func() error {
		sql := "SELECT count(*)" + scanSource(f) + where
		return s.pool.QueryRow(groupCtx, sql, b.args...).Scan(&total)
	})

	if err := group.Wait(); err != nil {
		return DisputeList{}, err
	}

	if list == nil {
		// An empty slice encodes as [], nil encodes as null. The client should
		// never have to handle both.
		list = []DisputeRow{}
	}

	return DisputeList{
		Rows:   list,
		Page:   Page{Offset: f.Offset, Limit: f.Limit, Total: total},
		TookMS: time.Since(started).Milliseconds(),
	}, nil
}

// StreamCSV writes the full filtered result set without buffering it.
//
// This is the answer to deep paging. The page query caps at 10,000 rows deep;
// an operator who genuinely wants 40,000 disputes gets them here, one row at a
// time from the database straight onto the socket, so memory stays flat
// whether the result is 40 rows or 400,000.
func (s *Store) StreamCSV(ctx context.Context, f Filters, w *csv.Writer) (int, error) {
	var b builder
	f.apply(&b, time.Now().UTC())

	rows, err := s.pool.Query(ctx, disputeSelect+b.where()+f.orderBy(), b.args...)
	if err != nil {
		return 0, fmt.Errorf("stream disputes: %w", err)
	}
	defer rows.Close()

	header := []string{
		"dispute_ref", "merchant", "merchant_name", "kind", "state", "card_network",
		"reason_code", "amount_minor", "currency", "opened_at", "deadline_at",
		"resolved_at", "transaction_ref", "card_last4", "customer_ref",
	}
	if err := w.Write(header); err != nil {
		return 0, err
	}

	count := 0
	for rows.Next() {
		d, err := scanDispute(rows)
		if err != nil {
			return count, fmt.Errorf("scan dispute: %w", err)
		}

		resolved := ""
		if d.ResolvedAt != nil {
			resolved = d.ResolvedAt.UTC().Format(time.RFC3339)
		}

		record := []string{
			d.ExternalID, d.MerchantID, d.MerchantName, d.Kind, d.State, d.CardNetwork,
			d.ReasonCode,
			// Minor units in the export too. A spreadsheet that reads "49.99"
			// as a float is exactly how a reconciliation goes wrong.
			strconv.FormatInt(d.Amount.AmountMinor, 10), d.Amount.Currency,
			d.OpenedAt.UTC().Format(time.RFC3339),
			d.DeadlineAt.UTC().Format(time.RFC3339),
			resolved, d.TransactionRef, d.CardLast4, d.CustomerRef,
		}
		if err := w.Write(record); err != nil {
			return count, err
		}
		count++

		// Flush periodically so the browser starts receiving immediately and
		// the whole export never sits in this process's memory.
		if count%1000 == 0 {
			w.Flush()
			if err := w.Error(); err != nil {
				return count, err
			}
		}
	}
	if err := rows.Err(); err != nil {
		return count, err
	}

	w.Flush()
	return count, w.Error()
}

// ---------------------------------------------------------------------------
// detail
// ---------------------------------------------------------------------------

type DisputeEvent struct {
	FromState  *string        `json:"from_state"`
	ToState    string         `json:"to_state"`
	Actor      string         `json:"actor"`
	Detail     map[string]any `json:"detail"`
	OccurredAt time.Time      `json:"occurred_at"`
}

type LedgerPosting struct {
	ExternalRef string    `json:"external_ref"`
	Kind        string    `json:"kind"`
	Account     string    `json:"account"`
	Direction   string    `json:"direction"`
	Amount      Money     `json:"amount"`
	OccurredAt  time.Time `json:"occurred_at"`
}

type DisputeDetail struct {
	DisputeRow
	CustomerEmail  string          `json:"customer_email"`
	Descriptor     string          `json:"descriptor"`
	CapturedAt     time.Time       `json:"captured_at"`
	OriginalAmount Money           `json:"original_amount"`
	RefundedMinor  int64           `json:"refunded_minor"`
	Events         []DisputeEvent  `json:"events"`
	LedgerPostings []LedgerPosting `json:"ledger_postings"`

	// CardholderClaim is the cardholder's own account, stored raw. It is the
	// only free text on a dispute and the only field written by the party
	// trying to reverse the charge, which is why it is deliberately absent
	// from the MCP tool output - that surface has nowhere to mark it as
	// untrusted. Reached through disputetools.DisputeWithClaim instead.
	CardholderClaim string `json:"cardholder_claim"`
}

func (s *Store) Dispute(ctx context.Context, id int64) (DisputeDetail, error) {
	var d DisputeDetail

	// Its own statement rather than disputeSelect with columns bolted on: the
	// shared string ends at the JOINs, so appending a projection to it produces
	// SQL that only fails once a request reaches it.
	const detailSQL = `
		SELECT d.id, d.external_id, m.external_id, m.name,
		       d.kind, d.state, d.card_network, d.reason_code,
		       d.amount_minor, d.currency,
		       d.opened_at, d.deadline_at, d.resolved_at,
		       t.external_id, t.card_last4, t.customer_ref,
		       EXTRACT(EPOCH FROM (d.deadline_at - now()))::bigint,
		       t.customer_email, t.descriptor, t.captured_at,
		       t.amount_minor, t.refunded_minor, d.cardholder_claim
		  FROM disputes d
		  JOIN merchants m    ON m.id = d.merchant_id
		  JOIN transactions t ON t.id = d.transaction_id
		 WHERE d.id = $1`

	err := s.pool.QueryRow(ctx, detailSQL, id).Scan(
		&d.ID, &d.ExternalID, &d.MerchantID, &d.MerchantName,
		&d.Kind, &d.State, &d.CardNetwork, &d.ReasonCode,
		&d.Amount.AmountMinor, &d.Amount.Currency,
		&d.OpenedAt, &d.DeadlineAt, &d.ResolvedAt,
		&d.TransactionRef, &d.CardLast4, &d.CustomerRef,
		&d.SecondsToDeadline,
		&d.CustomerEmail, &d.Descriptor, &d.CapturedAt,
		&d.OriginalAmount.AmountMinor, &d.RefundedMinor, &d.CardholderClaim,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return DisputeDetail{}, ErrNotFound
	}
	if err != nil {
		return DisputeDetail{}, fmt.Errorf("load dispute %d: %w", id, err)
	}
	d.OriginalAmount.Currency = d.Amount.Currency

	eventRows, err := s.pool.Query(ctx, `
		SELECT from_state, to_state, actor, detail, occurred_at
		  FROM dispute_events WHERE dispute_id = $1 ORDER BY occurred_at, id`, id)
	if err != nil {
		return DisputeDetail{}, fmt.Errorf("load dispute events: %w", err)
	}
	defer eventRows.Close()

	d.Events = []DisputeEvent{}
	for eventRows.Next() {
		var e DisputeEvent
		if err := eventRows.Scan(&e.FromState, &e.ToState, &e.Actor, &e.Detail, &e.OccurredAt); err != nil {
			return DisputeDetail{}, fmt.Errorf("scan dispute event: %w", err)
		}
		d.Events = append(d.Events, e)
	}
	if err := eventRows.Err(); err != nil {
		return DisputeDetail{}, err
	}

	// The money this dispute actually moved, read straight off the ledger
	// rather than recomputed. If the two ever disagree, the ledger is right.
	// The pattern is built here and bound as one text parameter.
	//
	// Written as `LIKE 'dispute:' || $1 || ':%'`, Postgres infers $1 as text
	// from the concatenation and pgx is holding an int64 - "unable to encode
	// 830 into text format for text (OID 25)". Neither a psql literal nor a
	// PREPARE with a declared bigint reproduces it, because both settle the
	// type the query itself leaves open.
	postingRows, err := s.pool.Query(ctx, `
		SELECT lt.external_ref, lt.kind, la.kind, le.direction, le.amount_minor, le.currency, lt.occurred_at
		  FROM ledger_transactions lt
		  JOIN ledger_entries le ON le.ledger_transaction_id = lt.id
		  JOIN ledger_accounts la ON la.id = le.account_id
		 WHERE lt.external_ref LIKE $1
		 ORDER BY lt.occurred_at, le.id`, fmt.Sprintf("dispute:%d:%%", id))
	if err != nil {
		return DisputeDetail{}, fmt.Errorf("load ledger postings: %w", err)
	}
	defer postingRows.Close()

	d.LedgerPostings = []LedgerPosting{}
	for postingRows.Next() {
		var p LedgerPosting
		if err := postingRows.Scan(&p.ExternalRef, &p.Kind, &p.Account, &p.Direction,
			&p.Amount.AmountMinor, &p.Amount.Currency, &p.OccurredAt); err != nil {
			return DisputeDetail{}, fmt.Errorf("scan ledger posting: %w", err)
		}
		d.LedgerPostings = append(d.LedgerPostings, p)
	}
	return d, postingRows.Err()
}

// CustomerHistoryRow is one prior dispute by the same customer.
type CustomerHistoryRow struct {
	ID         int64     `json:"id"`
	ExternalID string    `json:"external_id"`
	Kind       string    `json:"kind"`
	State      string    `json:"state"`
	ReasonCode string    `json:"reason_code"`
	Amount     Money     `json:"amount"`
	OpenedAt   time.Time `json:"opened_at"`
}

// CustomerHistory returns what this customer has disputed before.
//
// The single most useful signal when deciding whether to fight a dispute: a
// first-time claim reads very differently from a fifth. Scoped to one merchant
// because customer_ref is only unique within one.
func (s *Store) CustomerHistory(ctx context.Context, merchantExternalID, customerRef string, limit int) ([]CustomerHistoryRow, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT d.id, d.external_id, d.kind, d.state, d.reason_code,
		       d.amount_minor, d.currency, d.opened_at
		  FROM disputes d
		  JOIN merchants m    ON m.id = d.merchant_id
		  JOIN transactions t ON t.id = d.transaction_id
		 WHERE m.external_id = $1 AND t.customer_ref = $2
		 ORDER BY d.opened_at DESC
		 LIMIT $3`, merchantExternalID, customerRef, limit)
	if err != nil {
		return nil, fmt.Errorf("customer history: %w", err)
	}
	defer rows.Close()

	out := []CustomerHistoryRow{}
	for rows.Next() {
		var r CustomerHistoryRow
		if err := rows.Scan(&r.ID, &r.ExternalID, &r.Kind, &r.State, &r.ReasonCode,
			&r.Amount.AmountMinor, &r.Amount.Currency, &r.OpenedAt); err != nil {
			return nil, fmt.Errorf("scan customer history: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}
