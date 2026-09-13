// Package agent puts a model to work on one dispute: the representment flow.
//
// Assistant holds the dispute, FactSource assembles the record from the store
// (facts.go, evidence.go), Generator drafts a letter from it and Verifier
// checks the draft against the same record (generate.go, verify.go), and Runs
// writes what happened to agent_runs (store.go). The record is enriched before
// either call runs: Retriever finds settled disputes with a similar claim, by
// vector where an llm.Embedder is configured and by full-text search otherwise
// (precedent.go, embed backfill in backfill.go), and baserate.go adds how
// disputes like this one have gone at the merchant.
//
// Neither model call is given a tool that fetches anything, which is why this
// package does not use internal/toolloop: two judges need one record, and a
// generator that could look something up could cite what the verifier never
// saw. What both calls share is callTool in toolcall.go - one forced,
// single-turn call - and what they deliberately do not share is the validation
// after it. Draft.written and Verdict.checked are unexported and set only by
// the code that read the answer, so a zero value can never be mistaken for a
// result: fail-closed, because a verifier that fails open is worse than none.
//
// The transport is internal/llm, so the whole package can be exercised with a
// scripted completer (internal/llm/llmtest) and no API key.
// internal/agent/eval grades what the assistant produces.
package agent
