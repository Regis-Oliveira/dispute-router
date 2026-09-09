-- Demo data for the review queue: three drafts, one of each shape.
--
-- Not part of db/seed. The seed builds the dataset; this stages a scene, and
-- the difference matters because these letters were written by hand. Nothing
-- here came from a model - running the real agent is what cmd/agent does, and
-- it costs money. This exists so the screen can be shown and rehearsed for
-- nothing, and so a demo can be reset between takes.
--
-- Reset with: make demo-reset

-- Put back anything a previous run left mid-flow. agent_runs cannot be deleted
-- - the append-only trigger is doing its job - so the rows accumulate and only
-- the dispute states are rewound. That is honest: a demo run really did happen.
UPDATE disputes SET state = 'received', version = version + 1
 WHERE state = 'draft_ready';

WITH picked AS (
  SELECT id, row_number() OVER (ORDER BY deadline_at) AS n
    FROM disputes
   WHERE kind = 'chargeback' AND state = 'received' AND deadline_at > now()
     AND id NOT IN (SELECT dispute_id FROM agent_runs)
   ORDER BY deadline_at
   LIMIT 3
),
moved AS (
  UPDATE disputes d SET state = 'draft_ready', version = version + 1
    FROM picked p WHERE d.id = p.id
  RETURNING d.id, p.n
)
INSERT INTO agent_runs (dispute_id, attempt, model, prompt_fingerprint, tool_surface,
                        outcome, recommendation, letter, cited_evidence, findings,
                        input_tokens, output_tokens, cost_micros, trace, started_at)
SELECT m.id,
       CASE WHEN m.n = 2 THEN 2 ELSE 1 END,
       'demo.hand-written', 'sha256:demo0000',
       ARRAY['write_representment','record_verdict'],
       CASE m.n WHEN 1 THEN 'drafted' WHEN 2 THEN 'rejected' ELSE 'insufficient_evidence' END,
       CASE m.n WHEN 3 THEN 'insufficient_evidence' ELSE 'represent' END,
       CASE m.n
         WHEN 1 THEN E'To the issuing bank,\n\nThis charge was authorised by the cardholder through the merchant''s own checkout and settled without exception. The cardholder has filed no prior dispute with this merchant.\n\nReason code 10.4 alleges an unauthorised transaction. The record shows the statement descriptor matches the trading name, and the card details used match those on file.\n\nWe ask that the dispute be reversed.'
         WHEN 2 THEN E'To the issuing bank,\n\nThe goods were delivered on 14 February and signed for by the cardholder. Carrier tracking number 1Z999AA10123456784 confirms delivery to the billing address.\n\nWe ask that the dispute be reversed.'
         ELSE E'Nothing on file supports a rebuttal here. The dispute alleges the goods never arrived, and the merchant has uploaded no carrier confirmation, no proof of despatch, and no correspondence with the cardholder.\n\nTo contest this, the record would need a delivery confirmation naming the billing address, or evidence that the cardholder acknowledged receipt.'
       END,
       ARRAY[]::text[],
       CASE m.n WHEN 2 THEN '[{"check":"unsupported_claim","quote":"Carrier tracking number 1Z999AA10123456784","why":"no tracking number appears anywhere in the record"},{"check":"missing_evidence","quote":"signed for by the cardholder","why":"no signature or delivery document is on file for this dispute"}]'::jsonb
                            ELSE '[]'::jsonb END,
       3140, 412, 15420, '{"citations_ok":true,"note":"demo"}'::jsonb, now() - interval '4 minutes'
  FROM moved m;

SELECT a.id AS run, d.external_id AS dispute, a.outcome, a.attempt
  FROM agent_runs a JOIN disputes d ON d.id = a.dispute_id
 WHERE a.review IS NULL ORDER BY a.id;
