-- One balanced journal entry per capture:
--   debit  settlement_clearing   (the processor owes us this)
--   credit merchant_balance      (we owe the merchant this)
--
-- Set-based on purpose. Half a million journal entries posted row-by-row from
-- the application would take minutes; this takes seconds and is the same shape
-- a real backfill would use.

WITH posted AS (
  INSERT INTO ledger_transactions (external_ref, kind, currency, description, occurred_at)
  SELECT 'txn:' || t.id || ':capture', 'capture', t.currency, 'card capture', t.captured_at
    FROM transactions t
  ON CONFLICT (external_ref) DO NOTHING
  RETURNING id, external_ref
),
lines AS (
  SELECT p.id AS lt_id, t.merchant_id, t.amount_minor, t.currency
    FROM posted p
    JOIN transactions t ON t.id = split_part(p.external_ref, ':', 2)::bigint
)
INSERT INTO ledger_entries (ledger_transaction_id, account_id, direction, amount_minor, currency)
SELECT l.lt_id, a.id, 'debit', l.amount_minor, l.currency
  FROM lines l
  JOIN ledger_accounts a
    ON a.merchant_id IS NULL AND a.kind = 'settlement_clearing' AND a.currency = l.currency
UNION ALL
SELECT l.lt_id, a.id, 'credit', l.amount_minor, l.currency
  FROM lines l
  JOIN ledger_accounts a
    ON a.merchant_id = l.merchant_id AND a.kind = 'merchant_balance' AND a.currency = l.currency;
