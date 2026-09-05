// Package ledger writes the journal entries this platform posts.
//
// One place knows the chart of accounts. Before this existed the refund lived
// in the worker and the chargeback in the ingest service, which meant two
// packages independently deciding which account a debit lands in - and the day
// they disagreed, the books would still balance and still be wrong.
package ledger

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// ChargebackFeeMinor is what the network charges the merchant when a
// chargeback is lost, on top of the disputed amount.
const ChargebackFeeMinor int64 = 1500

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
	direction      string
	amountMinor    int64
}

// write inserts a journal entry and its legs.
//
// The balance is not checked here: the database checks it at COMMIT, for every
// entry, including ones written by code that has not been reviewed as carefully
// as this file.
func write(
	ctx context.Context, tx pgx.Tx,
	externalRef, kind, currency, description string,
	merchantID int64, occurredAt time.Time, metadata string,
	postings []posting,
) error {
	var ledgerTxID int64
	err := tx.QueryRow(ctx, `
		INSERT INTO ledger_transactions (external_ref, kind, currency, description, occurred_at, metadata)
		VALUES ($1, $2, $3::char(3), $4, $5, $6::jsonb)
		RETURNING id`,
		externalRef, kind, currency, description, occurredAt, metadata,
	).Scan(&ledgerTxID)
	if err != nil {
		return fmt.Errorf("post %s: %w", externalRef, err)
	}

	for _, p := range postings {
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
				ledgerTxID, p.direction, p.amountMinor, currency, merchantID, p.account)
		} else {
			_, result = tx.Exec(ctx, `
				INSERT INTO ledger_entries (ledger_transaction_id, account_id, direction, amount_minor, currency)
				SELECT $1::bigint, a.id, $2::text, $3::bigint, $4::char(3)
				  FROM ledger_accounts a
				 WHERE a.merchant_id IS NULL AND a.kind = $5::text AND a.currency = $4::char(3)`,
				ledgerTxID, p.direction, p.amountMinor, currency, p.account)
		}
		if result != nil {
			return fmt.Errorf("post %s leg %s/%s: %w", externalRef, p.direction, p.account, result)
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
func Hold(ctx context.Context, tx pgx.Tx, disputeID, merchantID, amountMinor int64,
	currency string, at time.Time, reasonCode string) error {
	return write(ctx, tx,
		ref(disputeID, "hold"), "chargeback", currency,
		"funds held pending the network's decision",
		merchantID, at, fmt.Sprintf(`{"reason_code":%q}`, reasonCode),
		[]posting{
			{account: "merchant_balance", merchantScoped: true, direction: "debit", amountMinor: amountMinor},
			{account: "disputes_payable", merchantScoped: true, direction: "credit", amountMinor: amountMinor},
		},
	)
}

// ReleaseHold records a representment the merchant won: the held amount goes
// back to their balance, and no fee is charged.
func ReleaseHold(ctx context.Context, tx pgx.Tx, disputeID, merchantID, amountMinor int64,
	currency string, at time.Time) error {
	return write(ctx, tx,
		ref(disputeID, "release"), "representment_won", currency,
		"representment won, hold released",
		merchantID, at, `{}`,
		[]posting{
			{account: "disputes_payable", merchantScoped: true, direction: "debit", amountMinor: amountMinor},
			{account: "merchant_balance", merchantScoped: true, direction: "credit", amountMinor: amountMinor},
		},
	)
}

// SettleLoss records a representment lost, or a window that closed with no
// response - which costs the merchant the same thing.
//
// Four legs: the hold is released and the money leaves for the issuer, and
// separately the merchant is charged the network's fee. Writing it as two legs
// with a netted amount would balance just as well and lose the fee, which is
// the number a merchant will eventually ask about.
func SettleLoss(ctx context.Context, tx pgx.Tx, disputeID, merchantID, amountMinor int64,
	currency string, at time.Time, why string) error {
	return write(ctx, tx,
		ref(disputeID, "chargeback"), "representment_lost", currency,
		why,
		merchantID, at, fmt.Sprintf(`{"fee_minor":%d}`, ChargebackFeeMinor),
		[]posting{
			{account: "disputes_payable", merchantScoped: true, direction: "debit", amountMinor: amountMinor},
			{account: "settlement_clearing", direction: "credit", amountMinor: amountMinor},
			{account: "merchant_balance", merchantScoped: true, direction: "debit", amountMinor: ChargebackFeeMinor},
			{account: "fee_revenue", direction: "credit", amountMinor: ChargebackFeeMinor},
		},
	)
}

// Refund records an alert refunded inside its window.
//
// No hold is involved: an alert is a warning, not a clawback. The merchant is
// choosing to give the money back before anybody takes it, which is exactly
// why it is the cheap outcome.
func Refund(ctx context.Context, tx pgx.Tx, disputeID, merchantID, amountMinor int64,
	currency string, at time.Time, reasonCode string) error {
	return write(ctx, tx,
		ref(disputeID, "refund"), "refund", currency,
		"auto-refund inside the alert window",
		merchantID, at, fmt.Sprintf(`{"reason_code":%q}`, reasonCode),
		[]posting{
			{account: "merchant_balance", merchantScoped: true, direction: "debit", amountMinor: amountMinor},
			{account: "settlement_clearing", direction: "credit", amountMinor: amountMinor},
		},
	)
}
