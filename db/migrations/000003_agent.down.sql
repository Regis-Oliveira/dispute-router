DROP TRIGGER IF EXISTS agent_runs_no_delete  ON agent_runs;
DROP TRIGGER IF EXISTS agent_runs_no_rewrite ON agent_runs;
DROP FUNCTION IF EXISTS agent_runs_no_delete();
DROP FUNCTION IF EXISTS agent_runs_are_append_only();

-- The delete trigger is dropped above, or this would refuse to run - which is
-- the trigger working, not a problem with it.
DROP TABLE IF EXISTS agent_runs;

DROP INDEX IF EXISTS disputes_draft_ready_deadline_idx;

-- Any dispute parked in the new state has to go somewhere the old constraint
-- allows, or adding it back fails on existing rows.
UPDATE disputes SET state = 'resolving' WHERE state = 'draft_ready';

ALTER TABLE disputes DROP CONSTRAINT disputes_state_check;

ALTER TABLE disputes ADD CONSTRAINT disputes_state_check
  CHECK (state IN (
    'received',
    'resolving',
    'refunded',
    'represented',
    'won',
    'lost',
    'expired'
  ));

ALTER TABLE disputes DROP COLUMN cardholder_claim;
