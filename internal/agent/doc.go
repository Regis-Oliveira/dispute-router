// Package agent puts a model to work on the dispute read model, in two shapes.
//
// The first is the representment assistant: Assistant holds one dispute,
// FactSource assembles the record from the store (facts.go, evidence.go),
// Generator drafts a letter from it and Verifier checks the draft against the
// same record (generate.go, verify.go), and Runs writes what happened to
// agent_runs (store.go). The record is enriched with precedent and base rates
// before either call runs: Retriever finds settled disputes with a similar
// claim, by vector where an Embedder is configured and by full-text search
// otherwise (precedent.go, embed.go, backfill.go), and baserate.go adds how
// disputes like this one have gone at the merchant.
//
// The second is the general loop: Loop drives a Completer over a Registry of
// read-only tools until the model stops or a Budget is spent (loop.go,
// tools.go). cmd/ask runs it; the assistant deliberately does not, because two
// judges need one record and a generator that can fetch could cite something
// the verifier never saw.
//
// The wire types are written out rather than imported from an SDK, so a second
// completer can share them and so the whole package can be exercised with a
// scripted completer (internal/agent/agenttest) and no API key.
// internal/agent/eval grades what the assistant produces.
package agent
