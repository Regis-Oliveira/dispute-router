//go:build security

// Package security is a black-box attack suite aimed at the running services
// over the loopback interface: the read API and the ingest webhook endpoint.
//
// It carries no production code. Everything here is a test, gated behind the
// `security` build tag so a plain `make go-test` never compiles or runs it; the
// suite is exercised on its own through `make attack`. Each test talks to a real
// service over HTTP rather than an in-process httptest server, because the
// properties it pins - unauthenticated access, CORS header composition, the body
// cap ahead of authentication - only exist once the whole middleware chain and
// the network are in the loop. A target that is not listening skips the test
// with a clear message, the way the repo's live tests skip on an unset
// DATABASE_URL, so a stopped service never fails the run.
package security
