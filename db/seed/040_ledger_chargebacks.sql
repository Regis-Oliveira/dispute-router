-- Chargeback accounting, in the same shape the services post at runtime.
--
-- A chargeback is a clawback that has already happened: the acquirer takes the
-- money when the network files it, and the outcome lands weeks later. So every
-- chargeback holds funds on arrival, and its resolution releases that hold in
-- one direction or the other.
--
-- Skipping the hold is the version of these books that balances and still lies:
-- a merchant's balance would show money that has already left the building.

-- 1. Hold, for every chargeback ever filed.
WITH posted AS (
  INSERT INTO ledger_transactions (external_ref, kind, currency, description, occurred_at, metadata)
  SELECT 'dispute:' || d.id || ':hold', 'chargeback', d.currency,
         'funds held pending the network''s decision', d.opened_at,
         jsonb_build_object('reason_code', d.reason_code)
    FROM disputes d
   WHERE d.kind = 'chargeback'
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
    ON a.merchant_id = l.merchant_id AND a.kind = 'disputes_payable' AND a.currency = l.currency;

-- 2. Release, for the ones the merchant won.
WITH posted AS (
  INSERT INTO ledger_transactions (external_ref, kind, currency, description, occurred_at)
  SELECT 'dispute:' || d.id || ':release', 'representment_won', d.currency,
         'representment won, hold released', d.resolved_at
    FROM disputes d
   WHERE d.kind = 'chargeback' AND d.state = 'won'
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
    ON a.merchant_id = l.merchant_id AND a.kind = 'disputes_payable' AND a.currency = l.currency
UNION ALL
SELECT l.lt_id, a.id, 'credit', l.amount_minor, l.currency
  FROM lines l
  JOIN ledger_accounts a
    ON a.merchant_id = l.merchant_id AND a.kind = 'merchant_balance' AND a.currency = l.currency;

-- 3. Settle, for the ones lost or left to expire. Four legs: the hold is
--    released and the money leaves for the issuer, and separately the merchant
--    is charged the network's fee. Netting those into two legs would balance
--    just as well and lose the fee, which is the number a merchant asks about.
WITH posted AS (
  INSERT INTO ledger_transactions (external_ref, kind, currency, description, occurred_at, metadata)
  SELECT 'dispute:' || d.id || ':chargeback', 'representment_lost', d.currency,
         CASE d.state WHEN 'lost' THEN 'representment lost'
                      ELSE 'response window closed with no representment' END,
         d.resolved_at,
         jsonb_build_object('fee_minor', 1500, 'reason_code', d.reason_code)
    FROM disputes d
   WHERE d.kind = 'chargeback' AND d.state IN ('lost', 'expired')
  ON CONFLICT (external_ref) DO NOTHING
  RETURNING id, external_ref
),
lines AS (
  SELECT p.id AS lt_id, d.merchant_id, d.amount_minor, d.currency, 1500::bigint AS fee_minor
    FROM posted p
    JOIN disputes d ON d.id = split_part(p.external_ref, ':', 2)::bigint
)
INSERT INTO ledger_entries (ledger_transaction_id, account_id, direction, amount_minor, currency)
SELECT l.lt_id, a.id, 'debit', l.amount_minor, l.currency
  FROM lines l
  JOIN ledger_accounts a
    ON a.merchant_id = l.merchant_id AND a.kind = 'disputes_payable' AND a.currency = l.currency
UNION ALL
SELECT l.lt_id, a.id, 'credit', l.amount_minor, l.currency
  FROM lines l
  JOIN ledger_accounts a
    ON a.merchant_id IS NULL AND a.kind = 'settlement_clearing' AND a.currency = l.currency
UNION ALL
SELECT l.lt_id, a.id, 'debit', l.fee_minor, l.currency
  FROM lines l
  JOIN ledger_accounts a
    ON a.merchant_id = l.merchant_id AND a.kind = 'merchant_balance' AND a.currency = l.currency
UNION ALL
SELECT l.lt_id, a.id, 'credit', l.fee_minor, l.currency
  FROM lines l
  JOIN ledger_accounts a
    ON a.merchant_id IS NULL AND a.kind = 'fee_revenue' AND a.currency = l.currency;
