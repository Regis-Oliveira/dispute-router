-- Rebuild the audit trail the seeded disputes would have produced: a 'received'
-- event when they arrived, and a terminal event for the ones already closed.

INSERT INTO dispute_events (dispute_id, from_state, to_state, actor, detail, occurred_at)
SELECT d.id, NULL, 'received', 'system',
       jsonb_build_object('source', 'seed', 'kind', d.kind, 'reason_code', d.reason_code),
       d.opened_at
  FROM disputes d;

INSERT INTO dispute_events (dispute_id, from_state, to_state, actor, detail, occurred_at)
SELECT d.id, 'received', d.state, 'system',
       jsonb_build_object('source', 'seed'),
       d.resolved_at
  FROM disputes d
 WHERE d.resolved_at IS NOT NULL;
