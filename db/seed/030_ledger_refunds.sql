-- A refunded alert reverses the capture, for the disputed amount only:
--   debit  merchant_balance      (we owe them less)
--   credit settlement_clearing   (the money goes back out)
--
-- Note the external_ref: 'dispute:<id>:refund' can be posted exactly once, so a
-- worker retrying after a crash cannot refund the same dispute twice.

WITH posted AS (
  INSERT INTO ledger_transactions (external_ref, kind, currency, description, occurred_at)
  SELECT 'dispute:' || d.id || ':refund', 'refund', d.currency,
         'refund issued against ' || d.kind, d.resolved_at
    FROM disputes d
   WHERE d.state = 'refunded'
  ON CONFLICT (external_ref) DO NOTHING
  RETURNING id, external_ref
),
lines AS (
  SELECT p.id AS lt_id, d.merchant_id, d.amount_minor, d.currency
    FROM posted p
    JOIN disputes d ON d.id = split_part(p.external_ref, ':', 2)::bigint
)
INSERT INTO ledger_entries (ledger_transaction_id, account_id, direction, amount_minor, currency)
SELECT l.lt_id, a.id, 'debit', l.amount_minor, l.currency
  FROM lines l
  JOIN ledger_accounts a
    ON a.merchant_id = l.merchant_id AND a.kind = 'merchant_balance' AND a.currency = l.currency
UNION ALL
SELECT l.lt_id, a.id, 'credit', l.amount_minor, l.currency
  FROM lines l
  JOIN ledger_accounts a
    ON a.merchant_id IS NULL AND a.kind = 'settlement_clearing' AND a.currency = l.currency;
