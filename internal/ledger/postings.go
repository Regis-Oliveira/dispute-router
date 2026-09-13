// Package ledger writes the journal entries this platform posts.
//
// One place knows the chart of accounts. Before this existed the refund lived
// in the worker and the chargeback in the ingest service, which meant two
// packages independently deciding which account a debit lands in - and the day
// they disagreed, the books would still balance and still be wrong.
package ledger

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// chargebackFeeMinor is what the network charges the merchant when a
// chargeback is lost, on top of the disputed amount.
const chargebackFeeMinor int64 = 1500

// Entry is the money one posting moves, and whose it is.
//
// A struct rather than parameters because the three numbers below used to be
// three adjacent int64 arguments: swapping two of them compiled, posted a
// merchant's money against the wrong account, and left books that balanced.
// Named fields are the only thing that makes that mistake visible at the call
// site, which in this package is the only place it can be caught at all.
type Entry struct {
	// DisputeID is the dispute the posting is about. It is also what makes
	// the external_ref unique, so it is what makes a retry idempotent.
	DisputeID int64

	// MerchantID selects the merchant-scoped accounts. The platform's own
	// accounts are the ones with no merchant at all and never use this.
	MerchantID int64

	// AmountMinor is the disputed amount in the currency's minor unit. The
	// network's fee is not part of it; SettleLoss adds that itself.
	AmountMinor int64

	// Currency is the ISO 4217 code, and it has to be the accounts' own: a leg
	// only finds an account whose currency matches, so a mismatch posts
	// nothing rather than posting somewhere plausible.
	Currency string

	// At is when the money moved, which is not always when it is written
	// down. Ingest passes the processor's own timestamp off the webhook so the
	// ledger is readable in the order things happened; the worker passes the
	// clock of the transaction it is already inside.
	At time.Time
}

// direction is which side of an account a leg lands on.
//
// A named type for the same reason Entry is a struct: "debit" and "credit"
// are one typo apart from each other and from nothing else, and the column
// they land in is text, so the database will take any word offered.
type direction string

const (
	debit  direction = "debit"
	credit direction = "credit"
)

// Every posting is idempotent through the unique external_ref on
// ledger_transactions: 'dispute:1234:hold' can exist exactly once. A retry
// after a commit that was never observed hits the constraint and rolls the
// whole transaction back rather than posting twice.
func ref(disputeID int64, kind string) string {
	return fmt.Sprintf("dispute:%d:%s", disputeID, kind)
}

// posting is one leg of an entry.
type posting struct {
	// account is a ledger_accounts.kind.
	account string
	// merchantScoped picks between the merchant's account and the platform's.
	merchantScoped bool
	direction      direction
	amountMinor    int64
}

// journal is the ledger_transactions row an Entry becomes, and the legs it
// splits into. Its three strings were three consecutive parameters, which is
// the hazard Entry exists to remove one level up.
type journal struct {
	// externalRef is the idempotency key; see ref.
	externalRef string
	// kind classifies the movement for reporting: "chargeback", "refund".
	kind string
	// description is the human-readable line for this entry.
	description string
	// metadata is encoded here rather than by the callers: a hand-built
	// literal with %q is Go quoting, not JSON escaping, and the two disagree
	// on exactly the characters a reason code from outside would carry.
	metadata any
	postings []posting
}

// write inserts a journal entry and its legs.
//
// The balance is not checked here: the database checks it at COMMIT, for every
// entry, including ones written by code that has not been reviewed as carefully
// as this file.
func write(ctx context.Context, tx pgx.Tx, e Entry, j journal) error {
	encoded, err := json.Marshal(j.metadata)
	if err != nil {
		return fmt.Errorf("post %s: encode metadata: %w", j.externalRef, err)
	}

	var ledgerTxID int64
	err = tx.QueryRow(ctx, `
		INSERT INTO ledger_transactions (external_ref, kind, currency, description, occurred_at, metadata)
		VALUES ($1, $2, $3::char(3), $4, $5, $6::jsonb)
		RETURNING id`,
		j.externalRef, j.kind, e.Currency, j.description, e.At, string(encoded),
	).Scan(&ledgerTxID)
	if err != nil {
		return fmt.Errorf("post %s: %w", j.externalRef, err)
	}

	for _, p := range j.postings {
		// Every parameter is cast. In an INSERT ... SELECT, Postgres does not
		// infer a parameter's type from the target column, and an untyped one
		// defaults to text - which is a runtime error, not a compile one.
		var result error
		if p.merchantScoped {
			_, result = tx.Exec(ctx, `
				INSERT INTO ledger_entries (ledger_transaction_id, account_id, direction, amount_minor, currency)
				SELECT $1::bigint, a.id, $2::text, $3::bigint, $4::char(3)
				  FROM ledger_accounts a
				 WHERE a.merchant_id = $5::bigint AND a.kind = $6::text AND a.currency = $4::char(3)`,
				ledgerTxID, p.direction, p.amountMinor, e.Currency, e.MerchantID, p.account)
		} else {
			_, result = tx.Exec(ctx, `
				INSERT INTO ledger_entries (ledger_transaction_id, account_id, direction, amount_minor, currency)
				SELECT $1::bigint, a.id, $2::text, $3::bigint, $4::char(3)
				  FROM ledger_accounts a
				 WHERE a.merchant_id IS NULL AND a.kind = $5::text AND a.currency = $4::char(3)`,
				ledgerTxID, p.direction, p.amountMinor, e.Currency, p.account)
		}
		if result != nil {
			return fmt.Errorf("post %s leg %s/%s: %w", j.externalRef, p.direction, p.account, result)
		}
	}
	return nil
}

// Hold records a chargeback arriving.
//
// The money is gone the moment the network files it - the acquirer takes it
// back immediately and the outcome is decided weeks later. So the merchant is
// owed less straight away, and the amount sits in disputes_payable, which is
// the platform's promise to return it if the merchant wins.
//
// Skipping this step is the version of these books that looks fine and lies:
// a merchant's balance would show money that has already left.
func Hold(ctx context.Context, tx pgx.Tx, e Entry, reasonCode string) error {
	return write(ctx, tx, e, journal{
		externalRef: ref(e.DisputeID, "hold"),
		kind:        "chargeback",
		description: "funds held pending the network's decision",
		metadata:    map[string]string{"reason_code": reasonCode},
		postings: []posting{
			{account: "merchant_balance", merchantScoped: true, direction: debit, amountMinor: e.AmountMinor},
			{account: "disputes_payable", merchantScoped: true, direction: credit, amountMinor: e.AmountMinor},
		},
	})
}

// ReleaseHold records a representment the merchant won: the held amount goes
// back to their balance, and no fee is charged.
func ReleaseHold(ctx context.Context, tx pgx.Tx, e Entry) error {
	return write(ctx, tx, e, journal{
		externalRef: ref(e.DisputeID, "release"),
		kind:        "representment_won",
		description: "representment won, hold released",
		metadata:    map[string]any{},
		postings: []posting{
			{account: "disputes_payable", merchantScoped: true, direction: debit, amountMinor: e.AmountMinor},
			{account: "merchant_balance", merchantScoped: true, direction: credit, amountMinor: e.AmountMinor},
		},
	})
}

// SettleLoss records a representment lost, or a window that closed with no
// response - which costs the merchant the same thing. why becomes the entry's
// description, so it is what somebody reading the books later sees.
//
// Four legs: the hold is released and the money leaves for the issuer, and
// separately the merchant is charged the network's fee. Writing it as two legs
// with a netted amount would balance just as well and lose the fee, which is
// the number a merchant will eventually ask about.
func SettleLoss(ctx context.Context, tx pgx.Tx, e Entry, why string) error {
	return write(ctx, tx, e, journal{
		externalRef: ref(e.DisputeID, "chargeback"),
		kind:        "representment_lost",
		description: why,
		metadata:    map[string]int64{"fee_minor": chargebackFeeMinor},
		postings: []posting{
			{account: "disputes_payable", merchantScoped: true, direction: debit, amountMinor: e.AmountMinor},
			{account: "settlement_clearing", direction: credit, amountMinor: e.AmountMinor},
			{account: "merchant_balance", merchantScoped: true, direction: debit, amountMinor: chargebackFeeMinor},
			{account: "fee_revenue", direction: credit, amountMinor: chargebackFeeMinor},
		},
	})
}

// Refund records an alert refunded inside its window.
//
// No hold is involved: an alert is a warning, not a clawback. The merchant is
// choosing to give the money back before anybody takes it, which is exactly
// why it is the cheap outcome.
func Refund(ctx context.Context, tx pgx.Tx, e Entry, reasonCode string) error {
	return write(ctx, tx, e, journal{
		externalRef: ref(e.DisputeID, "refund"),
		kind:        "refund",
		description: "auto-refund inside the alert window",
		metadata:    map[string]string{"reason_code": reasonCode},
		postings: []posting{
			{account: "merchant_balance", merchantScoped: true, direction: debit, amountMinor: e.AmountMinor},
			{account: "settlement_clearing", direction: credit, amountMinor: e.AmountMinor},
		},
	})
}
