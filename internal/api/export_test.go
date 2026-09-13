package api

// OutcomeRejected exposes outcomeRejected to the external test package.
//
// The pin against agent.OutcomeRejected cannot live in package api:
// internal/agent imports this package, so an internal test file importing the
// agent back is a cycle the test binary refuses. It lives in package api_test
// instead, which cannot see an unexported constant - hence this one line.
const OutcomeRejected = outcomeRejected
