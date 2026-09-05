-- Double-entry ledger.
--
-- Nothing in this system "adjusts a balance". Money moves by writing a balanced
-- journal entry: a set of postings whose debits equal its credits, in a single
-- currency, checked by the database at COMMIT. Balances are derived, never stored.
--
-- Three guarantees are enforced here rather than in application code, because
-- application code is where money bugs live:
--   1. every journal entry balances                  (deferred constraint trigger)
--   2. a posting's currency matches its account's    (composite foreign key)
--   3. postings are immutable                        (BEFORE UPDATE/DELETE trigger)

-- ---------------------------------------------------------------------------
-- accounts
-- ---------------------------------------------------------------------------

CREATE TABLE ledger_accounts (
  id             BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  -- NULL for platform-wide accounts (fee revenue, clearing).
  merchant_id    BIGINT REFERENCES merchants(id) ON DELETE RESTRICT,

  kind           TEXT    NOT NULL CHECK (kind IN (
                   'settlement_clearing',  -- asset:     money in flight from the processor
                   'merchant_balance',     -- liability: what we owe the merchant
                   'merchant_reserve',     -- liability: held back against future disputes
                   'disputes_payable',     -- liability: clawed back, not yet settled
                   'fee_revenue',          -- revenue:   what the platform charges
                   'dispute_losses'        -- expense:   chargebacks lost
                 )),

  -- Which direction increases this account. Asset/expense = debit, everything
  -- else = credit. The balance view uses it so signs come out human-readable.
  normal_balance TEXT    NOT NULL CHECK (normal_balance IN ('debit','credit')),
  currency       CHAR(3) NOT NULL CHECK (currency ~ '^[A-Z]{3}$'),

  created_at     TIMESTAMPTZ NOT NULL DEFAULT now(),

  -- One account per (merchant, kind, currency). NULLS NOT DISTINCT makes that
  -- hold for platform accounts too, where merchant_id is NULL.
  CONSTRAINT ledger_accounts_identity_key
    UNIQUE NULLS NOT DISTINCT (merchant_id, kind, currency),

  -- Target of the composite FK from ledger_entries. This is what makes
  -- "posting currency must equal account currency" a database guarantee.
  CONSTRAINT ledger_accounts_id_currency_key UNIQUE (id, currency)
);


-- ---------------------------------------------------------------------------
-- journal entries
-- ---------------------------------------------------------------------------

CREATE TABLE ledger_transactions (
  id           BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,

  -- The idempotency key for money. 'dispute:1234:refund' can only ever be
  -- posted once, so a worker that retries after a partial failure, or two
  -- workers racing the same dispute, cannot double-refund. A unique violation
  -- here is a successful outcome, not an error.
  external_ref TEXT        NOT NULL,

  kind         TEXT        NOT NULL CHECK (kind IN (
                 'capture','refund','chargeback','representment_won',
                 'representment_lost','fee','reserve_hold','reserve_release'
               )),
  currency     CHAR(3)     NOT NULL CHECK (currency ~ '^[A-Z]{3}$'),
  description  TEXT        NOT NULL DEFAULT '',
  metadata     JSONB       NOT NULL DEFAULT '{}'::jsonb,

  occurred_at  TIMESTAMPTZ NOT NULL,
  created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),

  CONSTRAINT ledger_transactions_external_ref_key UNIQUE (external_ref)
);

CREATE INDEX ledger_transactions_occurred_idx ON ledger_transactions (occurred_at DESC);


-- ---------------------------------------------------------------------------
-- postings
-- ---------------------------------------------------------------------------

CREATE TABLE ledger_entries (
  id                     BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  ledger_transaction_id  BIGINT  NOT NULL REFERENCES ledger_transactions(id) ON DELETE RESTRICT,
  account_id             BIGINT  NOT NULL,

  direction              TEXT    NOT NULL CHECK (direction IN ('debit','credit')),
  -- Always positive. Direction carries the sign; a negative amount is a bug
  -- disguised as a value.
  amount_minor           BIGINT  NOT NULL CHECK (amount_minor > 0),
  currency               CHAR(3) NOT NULL,

  created_at             TIMESTAMPTZ NOT NULL DEFAULT now(),

  CONSTRAINT ledger_entries_account_currency_fkey
    FOREIGN KEY (account_id, currency)
    REFERENCES ledger_accounts (id, currency)
);

CREATE INDEX ledger_entries_transaction_idx ON ledger_entries (ledger_transaction_id);
CREATE INDEX ledger_entries_account_idx     ON ledger_entries (account_id, created_at DESC);


-- ---------------------------------------------------------------------------
-- guarantee 1: every journal entry balances
-- ---------------------------------------------------------------------------
--
-- DEFERRABLE INITIALLY DEFERRED is the whole trick: the check runs at COMMIT,
-- not per row, so a transaction can insert the debit and the credit in any
-- order. Half a journal entry can exist mid-transaction; it can never be
-- committed.

CREATE OR REPLACE FUNCTION assert_ledger_balanced() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE
  txn_id   BIGINT := COALESCE(NEW.ledger_transaction_id, OLD.ledger_transaction_id);
  n_rows   INTEGER;
  delta    BIGINT;
BEGIN
  SELECT count(*),
         COALESCE(SUM(CASE WHEN direction = 'debit'
                           THEN amount_minor ELSE -amount_minor END), 0)
    INTO n_rows, delta
    FROM ledger_entries
   WHERE ledger_transaction_id = txn_id;

  -- The parent row was deleted in this same transaction; nothing to check.
  IF n_rows = 0 THEN
    RETURN NULL;
  END IF;

  IF n_rows < 2 THEN
    RAISE EXCEPTION
      'ledger transaction % has % posting(s); double-entry needs at least 2',
      txn_id, n_rows
      USING ERRCODE = 'check_violation';
  END IF;

  IF delta <> 0 THEN
    RAISE EXCEPTION
      'ledger transaction % is unbalanced by % (minor units): debits - credits <> 0',
      txn_id, delta
      USING ERRCODE = 'check_violation';
  END IF;

  RETURN NULL;
END;
$$;

CREATE CONSTRAINT TRIGGER ledger_entries_must_balance
  AFTER INSERT OR UPDATE OR DELETE ON ledger_entries
  DEFERRABLE INITIALLY DEFERRED
  FOR EACH ROW EXECUTE FUNCTION assert_ledger_balanced();


-- ---------------------------------------------------------------------------
-- guarantee 3: postings are immutable
-- ---------------------------------------------------------------------------
--
-- You do not edit history. A mistake is corrected by posting a reversing entry,
-- which leaves both the error and the fix visible in the audit trail.

CREATE OR REPLACE FUNCTION forbid_mutation() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
  RAISE EXCEPTION
    '% is append-only: post a reversing entry instead of running %',
    TG_TABLE_NAME, TG_OP
    USING ERRCODE = 'restrict_violation';
END;
$$;

CREATE TRIGGER ledger_entries_immutable
  BEFORE UPDATE OR DELETE ON ledger_entries
  FOR EACH ROW EXECUTE FUNCTION forbid_mutation();

CREATE TRIGGER ledger_transactions_immutable
  BEFORE UPDATE OR DELETE ON ledger_transactions
  FOR EACH ROW EXECUTE FUNCTION forbid_mutation();


-- ---------------------------------------------------------------------------
-- derived balances
-- ---------------------------------------------------------------------------
--
-- A view, not a column. There is no cached number to drift out of sync.
-- When this gets slow, the answer is a rollup table maintained by the same
-- transaction that writes the postings - not a nightly recalculation job.

CREATE VIEW ledger_account_balances AS
SELECT a.id                AS account_id,
       a.merchant_id,
       a.kind,
       a.currency,
       a.normal_balance,
       COALESCE(SUM(e.amount_minor) FILTER (WHERE e.direction = 'debit'),  0) AS debits_minor,
       COALESCE(SUM(e.amount_minor) FILTER (WHERE e.direction = 'credit'), 0) AS credits_minor,
       -- Signed so that "positive means more of what this account is for".
       CASE a.normal_balance
         WHEN 'debit' THEN
           COALESCE(SUM(CASE WHEN e.direction = 'debit'  THEN e.amount_minor ELSE -e.amount_minor END), 0)
         ELSE
           COALESCE(SUM(CASE WHEN e.direction = 'credit' THEN e.amount_minor ELSE -e.amount_minor END), 0)
       END AS balance_minor
  FROM ledger_accounts a
  LEFT JOIN ledger_entries e ON e.account_id = a.id
 GROUP BY a.id, a.merchant_id, a.kind, a.currency, a.normal_balance;
