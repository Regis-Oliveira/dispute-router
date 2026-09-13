// Package llm is the transport to the model providers: the wire types, the
// two clients, and the retry rule they share.
//
// Completer is the seam everything above this package talks through - Anthropic
// in production, a scripted double in the tests (internal/llm/llmtest) - and
// Embedder is the same seam for vectors, served by Voyage because Anthropic
// does not sell embeddings. Pricing turns a Usage into micro-dollars so a
// caller can bound what it spends.
//
// The wire types are written out rather than imported from an SDK. The reason
// is specific to this project rather than general: keeping them SDK-free lets a
// second completer share them and lets every package above be exercised with a
// scripted completer and no API key, which is what makes the eval set possible.
// In a greenfield application the SDK would be the default.
//
// What is deliberately not here: anything that knows what the model is being
// asked to do. The tool loop is internal/toolloop and the representment flow is
// internal/agent; this package only knows how to send a request and how to
// price the answer.
package llm
