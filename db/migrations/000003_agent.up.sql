-- ---------------------------------------------------------------------------
-- The representment assistant: what it was given, what it produced, and who
-- decided.
-- ---------------------------------------------------------------------------
--
-- A model wrote a letter that a merchant may send to a card network. If that
-- letter is ever questioned - by the merchant, by the network, by an auditor -
-- "the model wrote it" is not an answer. This migration exists so there is one.


-- ---------------------------------------------------------------------------
-- disputes.cardholder_claim
-- ---------------------------------------------------------------------------
--
-- Everything else on a dispute is a controlled vocabulary: reason codes, card
-- networks, states. This is the only free text, and the only field written by
-- the party trying to take the money back.
--
-- It is stored raw, exactly as it arrived. Sanitising on the way in would mean
-- the record no longer says what was actually claimed, and the record is the
-- point. It is quarantined where it is read instead - see Facts.render in
-- internal/agent/facts.go - because escaping belongs at the boundary that has a
-- format to escape for, not in the database.

ALTER TABLE disputes
  ADD COLUMN cardholder_claim TEXT NOT NULL DEFAULT '';


-- ---------------------------------------------------------------------------
-- disputes.state: 'draft_ready'
-- ---------------------------------------------------------------------------
--
-- A drafted representment awaiting a human. It sits between resolving and
-- represented, and it is deliberately not a terminal state: nothing the agent
-- produces resolves a dispute.

ALTER TABLE disputes DROP CONSTRAINT disputes_state_check;

ALTER TABLE disputes ADD CONSTRAINT disputes_state_check
  CHECK (state IN (
    'received',     -- ingested, not yet acted on
    'resolving',    -- a worker holds the lock and is deciding
    'draft_ready',  -- an agent drafted a representment; a human has not approved it
    'refunded',     -- money returned, dispute closed
    'represented',  -- evidence submitted, awaiting the network
    'won',
    'lost',
    'expired'       -- deadline passed with no action; a bug, not an outcome
  ));

-- The review queue, and the reason it is ordered by deadline: a draft nobody
-- approved is still money running out of time. The worker's own sweeper index
-- deliberately does not cover this state - a dispute waiting on a person must
-- not be picked up and drafted again - so without this index the case where a
-- reviewer goes on holiday is invisible until the disputes expire.
CREATE INDEX disputes_draft_ready_deadline_idx
  ON disputes (deadline_at)
  WHERE state = 'draft_ready';


-- ---------------------------------------------------------------------------
-- agent_runs
-- ---------------------------------------------------------------------------
--
-- One row per attempt at one dispute. It records what the agent was given as
-- much as what it produced, because a draft cannot be judged without knowing
-- which prompt and which tool surface produced it - and both of those change
-- while the rows stay.

CREATE TABLE agent_runs (
  id          BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  dispute_id  BIGINT      NOT NULL REFERENCES disputes(id) ON DELETE RESTRICT,

  -- Attempts are numbered, and (dispute_id, attempt) is unique. The same
  -- guarantee as ledger_transactions.external_ref, for the same reason: a
  -- retrying worker that has already paid for a draft must collide rather than
  -- pay again.
  attempt     INTEGER     NOT NULL CHECK (attempt > 0),

  model       TEXT        NOT NULL,

  -- A hash of the system prompts in force for this run. When a pass rate moves,
  -- the first question is whether the prompt moved, and a hash answers it
  -- without storing the same few kilobytes on every row.
  prompt_fingerprint TEXT NOT NULL,

  -- The tools the run was actually given, recorded rather than assumed from the
  -- code, because the code changes and this row has to stay true about the run
  -- it describes.
  tool_surface TEXT[]     NOT NULL DEFAULT '{}',

  outcome     TEXT        NOT NULL CHECK (outcome IN (
                'drafted',                -- written and the verifier passed it
                'rejected',               -- written and the verifier found something
                'insufficient_evidence',  -- the record does not support a rebuttal
                'budget_exceeded',
                'failed'                  -- no draft and no verdict; a fault, not an outcome
              )),

  recommendation TEXT     CHECK (recommendation IN ('represent','insufficient_evidence')),
  letter      TEXT        NOT NULL DEFAULT '',
  cited_evidence TEXT[]   NOT NULL DEFAULT '{}',

  -- Every finding, from the verifier and from the host-side citation check
  -- alike. Queryable so that "which rule do drafts break most often" is a
  -- question with an answer.
  findings    JSONB       NOT NULL DEFAULT '[]'::jsonb,

  input_tokens  INTEGER   NOT NULL DEFAULT 0 CHECK (input_tokens  >= 0),
  output_tokens INTEGER   NOT NULL DEFAULT 0 CHECK (output_tokens >= 0),

  -- Micro-dollars, matching internal/agent. Money is an integer here for the
  -- same reason it is everywhere else in this schema, and cents are too coarse:
  -- a run can legitimately cost a third of one.
  cost_micros BIGINT      NOT NULL DEFAULT 0 CHECK (cost_micros >= 0),

  -- The turn-by-turn record: what was called, what came back, how long it took.
  trace       JSONB       NOT NULL DEFAULT '{}'::jsonb,

  started_at  TIMESTAMPTZ NOT NULL,
  finished_at TIMESTAMPTZ NOT NULL DEFAULT now(),

  -- The human decision. Null until somebody makes one, and the only part of
  -- this row that may ever change.
  review      TEXT        CHECK (review IN ('submitted','discarded')),
  reviewed_by TEXT,
  reviewed_at TIMESTAMPTZ,

  CONSTRAINT agent_runs_dispute_attempt_key UNIQUE (dispute_id, attempt),

  CONSTRAINT agent_runs_review_is_complete CHECK (
    (review IS NULL) = (reviewed_by IS NULL) AND
    (review IS NULL) = (reviewed_at IS NULL)
  ),

  -- A run that produced no draft has nothing to review, and a run that produced
  -- one has a recommendation.
  CONSTRAINT agent_runs_draft_has_recommendation CHECK (
    (outcome IN ('drafted','rejected','insufficient_evidence')) = (recommendation IS NOT NULL)
  )
);

CREATE INDEX agent_runs_dispute_idx  ON agent_runs (dispute_id, attempt);
CREATE INDEX agent_runs_outcome_idx  ON agent_runs (outcome, finished_at DESC);

-- The review queue joins from here.
CREATE INDEX agent_runs_awaiting_review_idx
  ON agent_runs (finished_at)
  WHERE review IS NULL AND outcome = 'drafted';


-- ---------------------------------------------------------------------------
-- What a run says is immutable. What a person decided about it is not.
-- ---------------------------------------------------------------------------
--
-- The same rule the ledger enforces on postings, for a closer reason than it
-- looks: this table exists to answer "why did the system say that", and a table
-- whose rows can be edited afterwards cannot answer it. Nobody - not a
-- migration, not a fix-up script, not a well-meaning operator - gets to change
-- what the model produced once it is written.
--
-- The three review columns are exempt, because a decision arriving later is the
-- one thing that is supposed to happen to these rows.

CREATE FUNCTION agent_runs_are_append_only() RETURNS trigger AS $$
BEGIN
  IF (to_jsonb(OLD) - 'review' - 'reviewed_by' - 'reviewed_at')
     IS DISTINCT FROM
     (to_jsonb(NEW) - 'review' - 'reviewed_by' - 'reviewed_at') THEN
    RAISE EXCEPTION
      'agent_runs is append-only; only review, reviewed_by and reviewed_at may change (run %)', OLD.id;
  END IF;
  RETURN NEW;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER agent_runs_no_rewrite
  BEFORE UPDATE ON agent_runs
  FOR EACH ROW EXECUTE FUNCTION agent_runs_are_append_only();

CREATE FUNCTION agent_runs_no_delete() RETURNS trigger AS $$
BEGIN
  RAISE EXCEPTION 'agent_runs is append-only; run % cannot be deleted', OLD.id;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER agent_runs_no_delete
  BEFORE DELETE ON agent_runs
  FOR EACH ROW EXECUTE FUNCTION agent_runs_no_delete();
