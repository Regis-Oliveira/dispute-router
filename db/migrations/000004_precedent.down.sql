DROP INDEX IF EXISTS disputes_settled_precedent_idx;
DROP TABLE IF EXISTS dispute_embeddings;
DROP INDEX IF EXISTS disputes_claim_tsv_idx;
ALTER TABLE disputes DROP COLUMN IF EXISTS claim_tsv;

-- The extensions are left in place. Dropping vector would break any other
-- database in the cluster that uses it, and an extension nobody uses costs
-- nothing.
