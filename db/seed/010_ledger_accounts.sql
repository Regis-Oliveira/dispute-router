-- Chart of accounts. Platform accounts carry merchant_id NULL and exist once
-- per currency; the rest exist once per merchant.

INSERT INTO ledger_accounts (merchant_id, kind, normal_balance, currency)
SELECT NULL, k.kind, k.normal_balance, c.currency
  FROM (VALUES
          ('settlement_clearing', 'debit'),   -- asset:   money in flight
          ('fee_revenue',         'credit'),  -- revenue: what the platform charges
          ('dispute_losses',      'debit')    -- expense: chargebacks eaten
       ) AS k(kind, normal_balance)
  CROSS JOIN (SELECT DISTINCT currency FROM merchants) AS c
ON CONFLICT DO NOTHING;

INSERT INTO ledger_accounts (merchant_id, kind, normal_balance, currency)
SELECT m.id, k.kind, k.normal_balance, m.currency
  FROM merchants m
  CROSS JOIN (VALUES
          ('merchant_balance', 'credit'),     -- liability: what we owe them
          ('merchant_reserve', 'credit'),     -- liability: held against future disputes
          ('disputes_payable', 'credit')      -- liability: clawed back, unsettled
       ) AS k(kind, normal_balance)
ON CONFLICT DO NOTHING;
