-- ---------------------------------------------------------------------------
-- Precedent retrieval: what happened last time a claim like this one arrived.
-- ---------------------------------------------------------------------------
--
-- The single most useful thing a drafter can be told is that this merchant has
-- won a dispute like this before, and the record already knows - it just has no
-- way to be asked.
--
-- Two ways to ask, and both are built, because a vector index that has never
-- been compared against a lexical baseline is a claim rather than a result.
-- Sometimes plain full-text search wins, especially on short text in one
-- language, and a system that cannot tell has no business calling itself an
-- improvement.

CREATE EXTENSION IF NOT EXISTS vector;
CREATE EXTENSION IF NOT EXISTS pg_trgm;


-- ---------------------------------------------------------------------------
-- The lexical baseline
-- ---------------------------------------------------------------------------
--
-- Generated rather than maintained, so it cannot fall out of step with the
-- column it summarises. 'simple' rather than 'english': the claims are short,
-- and stemming a two-sentence complaint mostly discards the words that
-- distinguish it.

ALTER TABLE disputes
  ADD COLUMN claim_tsv tsvector
  GENERATED ALWAYS AS (to_tsvector('simple', coalesce(cardholder_claim, ''))) STORED;

CREATE INDEX disputes_claim_tsv_idx ON disputes USING gin (claim_tsv);


-- ---------------------------------------------------------------------------
-- The embeddings
-- ---------------------------------------------------------------------------
--
-- A separate table, not a column on disputes, for two reasons. Embedding is a
-- backfill that runs long after the row is written and may fail for one dispute
-- without touching it; and the model that produced a vector has to travel with
-- the vector, because two models' vectors are not comparable and mixing them
-- silently produces neighbours that are not neighbours.
--
-- The dimension is fixed at 1024 for voyage-3. That is worth knowing before
-- somebody swaps the model: the width is part of the schema, so changing
-- embedding models is a migration, not a configuration change.

CREATE TABLE dispute_embeddings (
  dispute_id BIGINT      PRIMARY KEY REFERENCES disputes(id) ON DELETE CASCADE,
  embedding  vector(1024) NOT NULL,
  model      TEXT        NOT NULL,
  -- What was embedded. The claim alone is the useful signal, but recording the
  -- source means a change of strategy is visible rather than inferred from a
  -- date.
  source     TEXT        NOT NULL DEFAULT 'cardholder_claim',
  created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- HNSW rather than IVFFlat: it needs no training pass, so it works on an empty
-- table and stays correct as rows arrive - which matters here because the
-- backfill is incremental and a dispute can be embedded the moment it lands.
--
-- Cosine distance, because the vectors are normalised and only direction
-- carries meaning.
CREATE INDEX dispute_embeddings_hnsw_idx
  ON dispute_embeddings USING hnsw (embedding vector_cosine_ops);

CREATE INDEX dispute_embeddings_model_idx ON dispute_embeddings (model);


-- ---------------------------------------------------------------------------
-- What makes a precedent
-- ---------------------------------------------------------------------------
--
-- Only disputes that reached an outcome are precedent; an open one has nothing
-- to teach yet. Partial, because that is a small slice of a large table and the
-- retrieval joins against it on every draft.

CREATE INDEX disputes_settled_precedent_idx
  ON disputes (merchant_id, reason_code, state)
  WHERE state IN ('won', 'lost');
