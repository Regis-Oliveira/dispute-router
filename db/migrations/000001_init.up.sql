-- Core domain: merchants, the card sales they took, and the disputes filed against them.
--
-- Money rules enforced everywhere in this file:
--   * amounts are BIGINT in the currency's minor unit (cents). Never NUMERIC, never float.
--   * every amount column carries a sibling currency column. There is no implicit USD.
--   * amounts are positive; direction lives in the ledger, not in a sign.

CREATE EXTENSION IF NOT EXISTS pgcrypto;

-- Shared helper: keeps updated_at honest without the application having to remember.
CREATE OR REPLACE FUNCTION set_updated_at() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
  NEW.updated_at := now();
  RETURN NEW;
END;
$$;


-- ---------------------------------------------------------------------------
-- merchants
-- ---------------------------------------------------------------------------

CREATE TABLE merchants (
  id             BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  external_id    TEXT        NOT NULL,
  name           TEXT        NOT NULL,
  -- HMAC key the simulator signs webhooks with and the ingest service verifies.
  webhook_secret TEXT        NOT NULL,
  currency       CHAR(3)     NOT NULL CHECK (currency ~ '^[A-Z]{3}$'),
  -- Auto-refund anything at or below this. NULL disables the automation.
  auto_refund_ceiling_minor BIGINT CHECK (auto_refund_ceiling_minor >= 0),
  created_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at     TIMESTAMPTZ NOT NULL DEFAULT now(),

  CONSTRAINT merchants_external_id_key UNIQUE (external_id)
);

CREATE TRIGGER merchants_set_updated_at
  BEFORE UPDATE ON merchants
  FOR EACH ROW EXECUTE FUNCTION set_updated_at();


-- ---------------------------------------------------------------------------
-- transactions: the original card sale a dispute points back at
-- ---------------------------------------------------------------------------

CREATE TABLE transactions (
  id             BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  merchant_id    BIGINT      NOT NULL REFERENCES merchants(id) ON DELETE RESTRICT,
  -- The processor's id for this charge. Unique per merchant, not globally.
  external_id    TEXT        NOT NULL,

  amount_minor   BIGINT      NOT NULL CHECK (amount_minor > 0),
  currency       CHAR(3)     NOT NULL CHECK (currency ~ '^[A-Z]{3}$'),
  -- Running total of money already given back. Can never exceed the sale.
  refunded_minor BIGINT      NOT NULL DEFAULT 0 CHECK (refunded_minor >= 0),

  status         TEXT        NOT NULL DEFAULT 'captured'
                   CHECK (status IN ('captured','partially_refunded','refunded')),

  card_network   TEXT        NOT NULL CHECK (card_network IN ('visa','mastercard','amex','discover')),
  card_bin       CHAR(6)     NOT NULL,
  card_last4     CHAR(4)     NOT NULL,

  customer_ref   TEXT        NOT NULL,
  customer_email TEXT        NOT NULL,

  descriptor     TEXT        NOT NULL,
  captured_at    TIMESTAMPTZ NOT NULL,
  created_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at     TIMESTAMPTZ NOT NULL DEFAULT now(),

  CONSTRAINT transactions_merchant_external_key UNIQUE (merchant_id, external_id),
  CONSTRAINT transactions_refund_not_over_capture CHECK (refunded_minor <= amount_minor)
);

CREATE TRIGGER transactions_set_updated_at
  BEFORE UPDATE ON transactions
  FOR EACH ROW EXECUTE FUNCTION set_updated_at();

-- The dashboard's default view: one merchant, newest first.
CREATE INDEX transactions_merchant_captured_idx
  ON transactions (merchant_id, captured_at DESC);

-- "Has this customer disputed before?" is the hottest question in the rules engine.
CREATE INDEX transactions_customer_idx
  ON transactions (merchant_id, customer_ref);


-- ---------------------------------------------------------------------------
-- disputes
-- ---------------------------------------------------------------------------
--
-- Two kinds arrive:
--   alert      - a pre-dispute warning (Visa RDR / Ethoca). Refund now and no
--                chargeback is ever filed. This is the window worth racing.
--   chargeback - the real thing. Money is already clawed back; you either
--                represent with evidence or eat it.

CREATE TABLE disputes (
  id            BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  merchant_id   BIGINT      NOT NULL REFERENCES merchants(id) ON DELETE RESTRICT,
  transaction_id BIGINT     NOT NULL REFERENCES transactions(id) ON DELETE RESTRICT,
  external_id   TEXT        NOT NULL,

  kind          TEXT        NOT NULL CHECK (kind IN ('alert','chargeback')),
  card_network  TEXT        NOT NULL CHECK (card_network IN ('visa','mastercard','amex','discover')),
  reason_code   TEXT        NOT NULL,

  amount_minor  BIGINT      NOT NULL CHECK (amount_minor > 0),
  currency      CHAR(3)     NOT NULL CHECK (currency ~ '^[A-Z]{3}$'),

  state         TEXT        NOT NULL DEFAULT 'received'
                  CHECK (state IN (
                    'received',     -- ingested, not yet acted on
                    'resolving',    -- a worker holds the lock and is deciding
                    'refunded',     -- money returned, dispute closed
                    'represented',  -- evidence submitted, awaiting the network
                    'won',
                    'lost',
                    'expired'       -- deadline passed with no action; a bug, not an outcome
                  )),

  -- The whole product is this column. Miss it and the option to act is gone.
  deadline_at   TIMESTAMPTZ NOT NULL,
  opened_at     TIMESTAMPTZ NOT NULL,
  resolved_at   TIMESTAMPTZ,

  -- Optimistic lock: workers bump this on every state change, so two workers
  -- racing the same dispute cannot both win.
  version       INTEGER     NOT NULL DEFAULT 0,

  created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at    TIMESTAMPTZ NOT NULL DEFAULT now(),

  CONSTRAINT disputes_merchant_external_key UNIQUE (merchant_id, external_id),
  CONSTRAINT disputes_resolved_has_timestamp CHECK (
    (state IN ('refunded','won','lost','expired')) = (resolved_at IS NOT NULL)
  )
);

CREATE TRIGGER disputes_set_updated_at
  BEFORE UPDATE ON disputes
  FOR EACH ROW EXECUTE FUNCTION set_updated_at();

-- Dashboard: filter by merchant + state, sort by newest.
CREATE INDEX disputes_merchant_state_opened_idx
  ON disputes (merchant_id, state, opened_at DESC);

-- The deadline sweeper only ever cares about still-open rows. A partial index
-- keeps it tiny no matter how much history accumulates.
CREATE INDEX disputes_open_deadline_idx
  ON disputes (deadline_at)
  WHERE state IN ('received','resolving');

CREATE INDEX disputes_transaction_idx ON disputes (transaction_id);


-- ---------------------------------------------------------------------------
-- dispute_events: append-only audit of every state change
-- ---------------------------------------------------------------------------

CREATE TABLE dispute_events (
  id          BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  dispute_id  BIGINT      NOT NULL REFERENCES disputes(id) ON DELETE RESTRICT,
  from_state  TEXT,
  to_state    TEXT        NOT NULL,
  -- 'system', 'worker:<id>', 'user:<id>' - who caused this.
  actor       TEXT        NOT NULL,
  detail      JSONB       NOT NULL DEFAULT '{}'::jsonb,
  occurred_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX dispute_events_dispute_idx ON dispute_events (dispute_id, occurred_at);


-- ---------------------------------------------------------------------------
-- webhook_events: raw, unparsed, append-only ingest log
-- ---------------------------------------------------------------------------
--
-- Written before anything is interpreted. If the parser has a bug you replay
-- from here; if a merchant argues about what they sent, this is the record.

CREATE TABLE webhook_events (
  id              BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  merchant_id     BIGINT REFERENCES merchants(id) ON DELETE RESTRICT,
  source          TEXT        NOT NULL,
  event_type      TEXT        NOT NULL,

  -- The dedupe key. Redis SETNX answers "seen it?" in microseconds; this
  -- unique index is the durable backstop when Redis has been flushed.
  idempotency_key TEXT        NOT NULL,

  payload         JSONB       NOT NULL,
  signature       TEXT        NOT NULL,

  status          TEXT        NOT NULL DEFAULT 'pending'
                    CHECK (status IN ('pending','processed','failed','duplicate')),
  attempts        INTEGER     NOT NULL DEFAULT 0,
  last_error      TEXT,

  received_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
  processed_at    TIMESTAMPTZ,

  CONSTRAINT webhook_events_idempotency_key UNIQUE (idempotency_key)
);

CREATE INDEX webhook_events_pending_idx
  ON webhook_events (received_at)
  WHERE status = 'pending';


-- ---------------------------------------------------------------------------
-- outbox: transactional outbox
-- ---------------------------------------------------------------------------
--
-- A worker cannot both commit a state change and publish to SQS atomically.
-- So it writes the message here in the same transaction, and a relay drains
-- the table into the queue. At-least-once delivery, zero lost messages.

CREATE TABLE outbox (
  id             BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  aggregate_type TEXT        NOT NULL,
  aggregate_id   BIGINT      NOT NULL,
  event_type     TEXT        NOT NULL,
  payload        JSONB       NOT NULL,
  created_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
  published_at   TIMESTAMPTZ
);

CREATE INDEX outbox_unpublished_idx
  ON outbox (created_at)
  WHERE published_at IS NULL;
