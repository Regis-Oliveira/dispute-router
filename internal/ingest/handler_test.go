package ingest

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"

	"github.com/regisoliveira/dispute-router/internal/dispute"
	"github.com/regisoliveira/dispute-router/internal/signing"
)

// The pipeline ServeHTTP documents is the security design, and until this file
// existed nothing exercised it from outside. Every test below goes in through
// httptest at the HTTP boundary, so what is pinned is the answer a sender gets,
// not the shape of an internal call.
//
// Two services are unavoidable. The first step of the pipeline is the per-IP
// limiter, so there is no request that reaches the handler without Redis, and
// the merchant lookup in step 3 is a real query, so there is none that gets
// past the body without Postgres. Both gates skip, the way the rest of the
// repo's live tests do. What runs with neither is at the bottom of the file:
// the two pure pieces of the pipeline, the router in step 2 and the client
// address step 1 keys on.

const (
	// testTolerance is the signature window every handler here is built with,
	// so a test can say "ten minutes old" and mean "outside it".
	testTolerance = 5 * time.Minute

	// testSecret is what the fake resolver hands back. It is never in the
	// merchants row: the handler resolves secrets separately, and a test that
	// read one from the database would be describing the arrangement Phase 2
	// deliberately took apart.
	testSecret = "whsec_handler_test"

	testBodyCap = 4096
)

// testRedis is database 14, not 15.
//
// internal/worker's tests own 15 and flush it, and `go test ./...` runs the two
// packages at the same time. Nothing in this file flushes anything: every
// bucket is uniquely prefixed and every idempotency key carries a TTL, so these
// tests destroy no state they did not create and need no build tag.
func testRedis(t *testing.T) *redis.Client {
	t.Helper()

	url := os.Getenv("REDIS_URL")
	if url == "" {
		t.Skip("REDIS_URL not set; skipping the ingest handler tests")
	}

	opts, err := redis.ParseURL(url)
	if err != nil {
		t.Fatalf("parse REDIS_URL: %v", err)
	}
	opts.DB = 14

	rdb := redis.NewClient(opts)
	if err := rdb.Ping(t.Context()).Err(); err != nil {
		t.Skipf("redis unreachable: %v", err)
	}
	t.Cleanup(func() { _ = rdb.Close() })
	return rdb
}

func testPool(t *testing.T) *pgxpool.Pool {
	t.Helper()

	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		t.Skip("DATABASE_URL not set; skipping the ingest handler tests")
	}

	pool, err := pgxpool.New(context.Background(), dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	if err := pool.Ping(t.Context()); err != nil {
		pool.Close()
		t.Skipf("postgres unreachable: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// fakeSecrets is the whole of the Resolver interface: merchant to keys.
type fakeSecrets map[string][]string

// SecretsFor answers from the map, and answers nothing at all - not an error -
// for a merchant it does not hold, which is what the real resolvers do.
func (f fakeSecrets) SecretsFor(_ context.Context, merchantExternalID string) ([]string, error) {
	return f[merchantExternalID], nil
}

// harness is one handler wired to a real Redis and a real Postgres, plus the
// merchant and transaction its deliveries point at.
type harness struct {
	t       *testing.T
	handler *Handler
	pool    *pgxpool.Pool
	rdb     *redis.Client
	guard   *Guard

	merchantID  int64
	merchant    string
	transaction string
	unique      string
	counter     int

	// clock is what HandlerOptions.Now reads. A test moves it instead of
	// sleeping, which is the reason the hook was added in 1.7.
	clock time.Time
}

// newHarness builds the handler under test; tweak replaces whichever option the
// test is about before it is constructed.
func newHarness(t *testing.T, tweak func(h *harness, opts *HandlerOptions)) *harness {
	t.Helper()

	h := &harness{
		t:     t,
		rdb:   testRedis(t),
		clock: time.Now().UTC().Truncate(time.Second),
	}
	h.pool = testPool(t)
	h.seed()

	h.guard = NewGuard(h.rdb, time.Minute)
	opts := HandlerOptions{
		Store:   NewStore(h.pool),
		Secrets: fakeSecrets{h.merchant: {testSecret}},
		Guard:   h.guard,
		// Buckets nobody else can name, so they need no cleanup. Cleaning up a
		// rate limiter with FLUSHDB is what makes a test destructive.
		MerchantLimiter: NewLimiter(h.rdb, uniquePrefix("merchant"), 6000, 100),
		IPLimiter:       NewLimiter(h.rdb, uniquePrefix("ip"), 6000, 100),
		Tolerance:       testTolerance,
		MaxBodyBytes:    testBodyCap,
		Logger:          slog.New(slog.NewTextHandler(io.Discard, nil)),
		Now:             func() time.Time { return h.clock },
	}
	if tweak != nil {
		tweak(h, &opts)
	}

	h.handler = NewHandler(opts)
	return h
}

func uniquePrefix(kind string) string {
	return fmt.Sprintf("test:ingest:%s:%d", kind, time.Now().UnixNano())
}

// seed puts one merchant and one transaction on file and arranges for both, and
// anything the handler writes against them, to be removed again.
func (h *harness) seed() {
	h.t.Helper()
	ctx := h.t.Context()

	h.unique = strconv.FormatInt(time.Now().UnixNano(), 10)
	h.merchant = "mrc_handler_test_" + h.unique
	h.transaction = "txn_handler_test_" + h.unique

	if err := h.pool.QueryRow(ctx, `
		INSERT INTO merchants (external_id, name, webhook_secret, currency, auto_refund_ceiling_minor)
		VALUES ($1, 'Handler Test', 'unused: the resolver answers', 'USD', 5000)
		RETURNING id`, h.merchant,
	).Scan(&h.merchantID); err != nil {
		h.t.Fatalf("seed merchant: %v", err)
	}
	// Registered before the second insert, so a failure there still takes the
	// merchant back out.
	h.t.Cleanup(h.cleanup)

	if _, err := h.pool.Exec(ctx, `
		INSERT INTO transactions (
			merchant_id, external_id, amount_minor, currency, card_network,
			card_bin, card_last4, customer_ref, customer_email, descriptor, captured_at)
		VALUES ($1, $2, 25000, 'USD', 'visa', '411111', '4242',
		        'cus_handler_test', 'handler@example.com', 'HANDLER TEST',
		        now() - interval '10 days')`,
		h.merchantID, h.transaction,
	); err != nil {
		h.t.Fatalf("seed transaction: %v", err)
	}
}

// cleanup removes exactly the rows this harness caused, which is why these
// tests carry no build tag: they create no database and drop none.
//
// Every delivery sent below is an alert, never a chargeback, and that is not
// incidental: a chargeback posts to the ledger, the ledger is append-only by
// trigger, and a test that wrote one could not undo itself.
func (h *harness) cleanup() {
	// t.Context() is already cancelled by the time cleanups run.
	ctx := context.Background()

	for _, statement := range []string{
		`DELETE FROM outbox WHERE aggregate_type = 'dispute'
		   AND aggregate_id IN (SELECT id FROM disputes WHERE merchant_id = $1)`,
		`DELETE FROM dispute_events
		  WHERE dispute_id IN (SELECT id FROM disputes WHERE merchant_id = $1)`,
		`DELETE FROM disputes WHERE merchant_id = $1`,
		`DELETE FROM webhook_events WHERE merchant_id = $1`,
		`DELETE FROM transactions WHERE merchant_id = $1`,
		`DELETE FROM merchants WHERE id = $1`,
	} {
		if _, err := h.pool.Exec(ctx, statement, h.merchantID); err != nil {
			h.t.Errorf("cleanup left rows behind for merchant %d: %v", h.merchantID, err)
			return
		}
	}
}

// ids hands out a fresh event and dispute id, so no two deliveries in a test
// collide on the idempotency key or the merchant's dispute uniqueness.
func (h *harness) ids() (eventID, disputeID string) {
	h.counter++
	return fmt.Sprintf("evt_%s_%d", h.unique, h.counter),
		fmt.Sprintf("dsp_%s_%d", h.unique, h.counter)
}

func (h *harness) disputeBody(eventID, disputeID string, mutate func(*DisputeWebhook)) []byte {
	h.t.Helper()

	event := DisputeWebhook{
		ID:        eventID,
		Type:      TypeDisputeOpened,
		CreatedAt: h.clock,
		Data: DisputeData{
			DisputeID:     disputeID,
			MerchantID:    h.merchant,
			TransactionID: h.transaction,
			Kind:          string(dispute.KindAlert),
			CardNetwork:   "visa",
			ReasonCode:    "10.4",
			AmountMinor:   4999,
			Currency:      "USD",
			OpenedAt:      h.clock.Add(-time.Hour),
			RespondBy:     h.clock.Add(72 * time.Hour),
		},
	}
	if mutate != nil {
		mutate(&event)
	}

	raw, err := json.Marshal(event)
	if err != nil {
		h.t.Fatalf("marshal dispute webhook: %v", err)
	}
	return raw
}

func (h *harness) rulingBody(eventID, disputeID string) []byte {
	h.t.Helper()

	raw, err := json.Marshal(RulingWebhook{
		ID:        eventID,
		Type:      TypeDisputeResolved,
		CreatedAt: h.clock,
		Data: RulingData{
			DisputeID:  disputeID,
			MerchantID: h.merchant,
			Outcome:    string(dispute.StateWon),
			DecidedAt:  h.clock,
			Note:       "compelling evidence",
		},
	})
	if err != nil {
		h.t.Fatalf("marshal ruling webhook: %v", err)
	}
	return raw
}

// sign produces the header a well-behaved sender would send, at the handler's
// current clock.
func (h *harness) sign(body []byte) string {
	return signing.Sign(testSecret, body, h.clock)
}

func (h *harness) post(body []byte, signature string) *httptest.ResponseRecorder {
	h.t.Helper()
	return h.postReader(bytes.NewReader(body), signature)
}

func (h *harness) postReader(body io.Reader, signature string) *httptest.ResponseRecorder {
	h.t.Helper()

	req := httptest.NewRequestWithContext(h.t.Context(), http.MethodPost, "/webhooks/processor", body)
	if signature != "" {
		req.Header.Set(signatureHeader, signature)
	}
	rec := httptest.NewRecorder()
	h.handler.ServeHTTP(rec, req)
	return rec
}

func (h *harness) count(query string, args ...any) int {
	h.t.Helper()

	var n int
	if err := h.pool.QueryRow(h.t.Context(), query, args...).Scan(&n); err != nil {
		h.t.Fatalf("count: %v", err)
	}
	return n
}

func assertStatus(t *testing.T, rec *httptest.ResponseRecorder, want int) {
	t.Helper()
	if rec.Code != want {
		t.Fatalf("status = %d, want %d; body %s", rec.Code, want, strings.TrimSpace(rec.Body.String()))
	}
}

func assertBody(t *testing.T, rec *httptest.ResponseRecorder, want string) {
	t.Helper()
	if got := strings.TrimSpace(rec.Body.String()); got != want {
		t.Errorf("body = %s, want %s", got, want)
	}
}

// errReader is a client that hangs up mid-body. io.ReadAll then returns an
// error that is not *http.MaxBytesError, which is the whole of the distinction
// 1.7 fixed: that request never hit a size limit and must not be told it did.
type errReader struct{}

func (errReader) Read([]byte) (int, error) { return 0, errors.New("connection reset by peer") }

// Each subtest breaks two of the pipeline's rules at once and asserts that the
// earlier rule is the one that answers. Any of them failing means the order in
// ServeHTTP's doc comment has drifted from the code under it.
func TestServeHTTPEnforcesTheDocumentedOrder(t *testing.T) {
	t.Run("the ip limit is spent before the body is read", func(t *testing.T) {
		h := newHarness(t, func(h *harness, opts *HandlerOptions) {
			opts.IPLimiter = NewLimiter(h.rdb, uniquePrefix("ip"), 1, 1)
		})

		// The one token in the bucket.
		assertStatus(t, h.post([]byte(`{`), "t=1,v1=00"), http.StatusBadRequest)

		// Over the cap and out of budget. The budget answers.
		assertStatus(t, h.post(bytes.Repeat([]byte("a"), testBodyCap+1), ""), http.StatusTooManyRequests)
	})

	t.Run("the body cap is checked before the signature", func(t *testing.T) {
		h := newHarness(t, nil)

		// No signature header at all, and a body over the cap.
		assertStatus(t, h.post(bytes.Repeat([]byte("a"), testBodyCap+1), ""), http.StatusRequestEntityTooLarge)
	})

	t.Run("a missing signature is answered before the body is parsed", func(t *testing.T) {
		h := newHarness(t, nil)

		rec := h.post([]byte(`not json at all`), "")
		assertStatus(t, rec, http.StatusUnauthorized)
		assertBody(t, rec, `{"error":"missing signature"}`)
	})

	t.Run("the body is parsed before the signature is verified", func(t *testing.T) {
		// Step 2 before step 3, which the comment calls unavoidable: the
		// merchant id that chooses the key is inside the body. Nothing from the
		// parse is acted on, so answering 400 here leaks nothing.
		h := newHarness(t, nil)

		assertStatus(t, h.post([]byte(`{"id":"evt_x","type":"dispute.opened"`), "t=1,v1=00"), http.StatusBadRequest)
	})

	t.Run("the signature is verified before the merchant's budget is spent", func(t *testing.T) {
		h := newHarness(t, func(h *harness, opts *HandlerOptions) {
			// A bucket that can never hand out a token: if step 4 ran at all it
			// would answer 429.
			opts.MerchantLimiter = NewLimiter(h.rdb, uniquePrefix("merchant"), 1, 0)
		})

		eventID, disputeID := h.ids()
		body := h.disputeBody(eventID, disputeID, nil)
		assertStatus(t, h.post(body, signing.Sign("whsec_not_the_key", body, h.clock)), http.StatusUnauthorized)
	})

	t.Run("a rejected signature does not spend the merchant's budget", func(t *testing.T) {
		// The reason step 4 is after step 3 rather than beside it: anyone who
		// can reach the endpoint could otherwise exhaust a merchant's budget
		// with garbage and take the merchant's real deliveries down with it.
		h := newHarness(t, func(h *harness, opts *HandlerOptions) {
			opts.MerchantLimiter = NewLimiter(h.rdb, uniquePrefix("merchant"), 1, 1)
		})

		eventID, disputeID := h.ids()
		body := h.disputeBody(eventID, disputeID, nil)
		assertStatus(t, h.post(body, signing.Sign("whsec_not_the_key", body, h.clock)), http.StatusUnauthorized)

		// The single token is still there for the delivery that is really from
		// this merchant.
		assertStatus(t, h.post(body, h.sign(body)), http.StatusAccepted)
	})

	t.Run("the merchant's budget is spent before the idempotency key is claimed", func(t *testing.T) {
		h := newHarness(t, func(h *harness, opts *HandlerOptions) {
			opts.MerchantLimiter = NewLimiter(h.rdb, uniquePrefix("merchant"), 1, 0)
		})

		eventID, disputeID := h.ids()
		body := h.disputeBody(eventID, disputeID, nil)
		assertStatus(t, h.post(body, h.sign(body)), http.StatusTooManyRequests)

		// A claim taken here would be a claim nothing committed: the sender's
		// retry would be answered "already handled" and the event lost.
		claimed, err := h.rdb.Exists(t.Context(), h.guard.key(eventID)).Result()
		if err != nil {
			t.Fatalf("EXISTS: %v", err)
		}
		if claimed != 0 {
			t.Error("a rate-limited delivery claimed its idempotency key")
		}
	})
}

// Step 3 of the pipeline, and the reason it is one step rather than two.
// Different answers for "no such merchant" and "wrong key" turn the endpoint
// into a merchant-id oracle: anyone could sweep for ids that exist.
func TestUnknownMerchantAndBadSignatureAreIndistinguishable(t *testing.T) {
	h := newHarness(t, nil)

	unknownEvent, unknownDispute := h.ids()
	unknown := h.disputeBody(unknownEvent, unknownDispute, func(e *DisputeWebhook) {
		e.Data.MerchantID = "mrc_not_on_file_" + h.unique
	})
	// Correctly signed with a real key. Only the merchant is unknown.
	unknownRec := h.post(unknown, signing.Sign(testSecret, unknown, h.clock))

	wrongKeyEvent, wrongKeyDispute := h.ids()
	wrongKey := h.disputeBody(wrongKeyEvent, wrongKeyDispute, nil)
	// A merchant that is on file, signed with a key that is not theirs.
	wrongKeyRec := h.post(wrongKey, signing.Sign("whsec_not_the_key", wrongKey, h.clock))

	assertStatus(t, unknownRec, http.StatusUnauthorized)
	assertStatus(t, wrongKeyRec, http.StatusUnauthorized)

	if unknownRec.Body.String() != wrongKeyRec.Body.String() {
		t.Errorf("the two rejections differ: unknown merchant %s, bad signature %s",
			strings.TrimSpace(unknownRec.Body.String()), strings.TrimSpace(wrongKeyRec.Body.String()))
	}
	assertBody(t, unknownRec, `{"error":"invalid signature"}`)

	if strings.Contains(strings.ToLower(unknownRec.Body.String()), "merchant") {
		t.Error("the rejection names the merchant, which is the oracle this avoids")
	}
	if got, want := unknownRec.Header().Get("Content-Type"), wrongKeyRec.Header().Get("Content-Type"); got != want {
		t.Errorf("content types differ: %q and %q", got, want)
	}

	// Neither delivery reached the write path.
	if n := h.count(`SELECT count(*) FROM webhook_events WHERE merchant_id = $1`, h.merchantID); n != 0 {
		t.Errorf("%d webhook_events written by rejected deliveries, want 0", n)
	}
}

// The distinction 1.7 introduced. Before it, any failure to read the body -
// including a client that hung up - was answered 413, which is a lie about a
// limit the sender never hit.
func TestBodyTooLargeAndBodyUnreadableAreDifferentAnswers(t *testing.T) {
	t.Run("over the cap is 413", func(t *testing.T) {
		h := newHarness(t, nil)

		rec := h.post(bytes.Repeat([]byte("a"), testBodyCap+1), "t=1,v1=00")
		assertStatus(t, rec, http.StatusRequestEntityTooLarge)
		assertBody(t, rec, `{"error":"body too large"}`)
	})

	t.Run("exactly at the cap is not too large", func(t *testing.T) {
		h := newHarness(t, nil)

		// It is nowhere near valid JSON, so 400 - but from the parser, not the
		// cap. An off-by-one in MaxBytesReader would show up here as a 413.
		rec := h.post(bytes.Repeat([]byte("a"), testBodyCap), "t=1,v1=00")
		assertStatus(t, rec, http.StatusBadRequest)
	})

	t.Run("a body that stops mid-stream is 400", func(t *testing.T) {
		h := newHarness(t, nil)

		rec := h.postReader(io.MultiReader(strings.NewReader(`{"id":"evt`), errReader{}), "t=1,v1=00")
		assertStatus(t, rec, http.StatusBadRequest)
		assertBody(t, rec, `{"error":"could not read body"}`)
	})

	t.Run("a body that arrives whole but truncated is 400", func(t *testing.T) {
		h := newHarness(t, nil)

		rec := h.post([]byte(`{"id":"evt_x","type":"dispute.opened","data":{`), "t=1,v1=00")
		assertStatus(t, rec, http.StatusBadRequest)
		// The parser's reason, not the reader's.
		if body := rec.Body.String(); strings.Contains(body, "could not read body") {
			t.Errorf("a parse failure was reported as a read failure: %s", strings.TrimSpace(body))
		}
	})
}

// The tolerance window, decided by the injected clock rather than by waiting.
// Nothing about the request changes between the three calls below, so the clock
// is the only thing that can be deciding.
func TestTheClockDecidesWhetherASignatureIsStale(t *testing.T) {
	h := newHarness(t, nil)

	eventID, disputeID := h.ids()
	body := h.disputeBody(eventID, disputeID, nil)
	signedAt := h.clock
	signature := signing.Sign(testSecret, body, signedAt)

	// Twice the tolerance later: a captured delivery replayed tomorrow is not
	// accepted today's window.
	h.clock = signedAt.Add(2 * testTolerance)
	rec := h.post(body, signature)
	assertStatus(t, rec, http.StatusUnauthorized)
	assertBody(t, rec, `{"error":"invalid signature"}`)

	// A timestamp from the future is the same answer: a sender with a skewed
	// clock cannot mint signatures that stay valid.
	h.clock = signedAt.Add(-2 * testTolerance)
	assertStatus(t, h.post(body, signature), http.StatusUnauthorized)

	// Inside the window, the very same bytes are accepted.
	h.clock = signedAt
	assertStatus(t, h.post(body, signature), http.StatusAccepted)

	// And neither rejection claimed the key, so the accepted one was first.
	if n := h.count(
		`SELECT count(*) FROM webhook_events WHERE idempotency_key = $1`, eventID,
	); n != 1 {
		t.Errorf("%d deliveries recorded, want the single accepted one", n)
	}
}

// Step 5. The second delivery is answered from Redis without the store being
// asked anything, which is the point of the guard - and the durable counts say
// the work behind the first one happened exactly once.
func TestAReplayedEventIsAcceptedOnceAndWrittenOnce(t *testing.T) {
	h := newHarness(t, nil)

	eventID, disputeID := h.ids()
	body := h.disputeBody(eventID, disputeID, nil)
	signature := h.sign(body)

	first := h.post(body, signature)
	assertStatus(t, first, http.StatusAccepted)

	var accepted struct {
		Status    string `json:"status"`
		EventID   string `json:"event_id"`
		DisputeID int64  `json:"dispute_id"`
	}
	if err := json.Unmarshal(first.Body.Bytes(), &accepted); err != nil {
		t.Fatalf("decode the first answer: %v", err)
	}
	if accepted.Status != "accepted" || accepted.EventID != eventID || accepted.DisputeID == 0 {
		t.Fatalf("first answer = %+v, want accepted with a dispute id", accepted)
	}

	second := h.post(body, signature)
	assertStatus(t, second, http.StatusOK)

	var duplicate struct {
		Status  string `json:"status"`
		EventID string `json:"event_id"`
	}
	if err := json.Unmarshal(second.Body.Bytes(), &duplicate); err != nil {
		t.Fatalf("decode the second answer: %v", err)
	}
	if duplicate.Status != "duplicate" || duplicate.EventID != eventID {
		t.Errorf("second answer = %+v, want duplicate", duplicate)
	}

	if n := h.count(`SELECT count(*) FROM webhook_events WHERE idempotency_key = $1`, eventID); n != 1 {
		t.Errorf("%d webhook_events for one event id, want 1", n)
	}
	if n := h.count(
		`SELECT count(*) FROM disputes WHERE merchant_id = $1 AND external_id = $2`,
		h.merchantID, disputeID,
	); n != 1 {
		t.Errorf("%d disputes, want 1", n)
	}
	if n := h.count(
		`SELECT count(*) FROM dispute_events WHERE dispute_id = $1`, accepted.DisputeID,
	); n != 1 {
		t.Errorf("%d dispute_events, want the single 'received' entry", n)
	}
	if n := h.count(
		`SELECT count(*) FROM outbox WHERE aggregate_type = 'dispute' AND aggregate_id = $1`,
		accepted.DisputeID,
	); n != 1 {
		t.Errorf("%d outbox rows, want 1", n)
	}
}

// The other delivery shape, all the way through the same pipeline, ending in
// the 422 that tells a sender its retry will not help. The delivery is on disk
// either way: a ruling nobody can act on is still a thing somebody will ask
// about later.
func TestARulingNothingIsAwaitingIsUnprocessableAndStillRecorded(t *testing.T) {
	h := newHarness(t, nil)

	eventID, disputeID := h.ids()
	body := h.rulingBody(eventID, disputeID)

	rec := h.post(body, h.sign(body))
	assertStatus(t, rec, http.StatusUnprocessableEntity)

	var answer struct {
		Status string `json:"status"`
		Reason string `json:"reason"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &answer); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if answer.Status != "unlinkable" || answer.Reason == "" {
		t.Errorf("answer = %+v, want unlinkable with a reason", answer)
	}

	var status string
	if err := h.pool.QueryRow(h.t.Context(),
		`SELECT status FROM webhook_events WHERE idempotency_key = $1`, eventID,
	).Scan(&status); err != nil {
		t.Fatalf("load the recorded delivery: %v", err)
	}
	if status != "failed" {
		t.Errorf("webhook_events.status = %q, want failed", status)
	}
}

// The two pieces of the pipeline that need no service at all, so they run with
// REDIS_URL and DATABASE_URL both unset.

func TestClientIPDropsThePort(t *testing.T) {
	t.Parallel()

	cases := map[string]struct{ remote, want string }{
		"host and port":    {"192.0.2.1:54321", "192.0.2.1"},
		"ipv6 and port":    {"[2001:db8::1]:443", "2001:db8::1"},
		"no port at all":   {"192.0.2.9", "192.0.2.9"},
		"unix socket peer": {"@", "@"},
	}

	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			req := httptest.NewRequest(http.MethodPost, "/webhooks/processor", nil)
			req.RemoteAddr = c.remote
			if got := clientIP(req); got != c.want {
				t.Errorf("clientIP(%q) = %q, want %q", c.remote, got, c.want)
			}
		})
	}
}

// Step 2 routes on the type and nothing else, and hands back the delivery the
// rest of the pipeline talks to: the idempotency key and the claimed merchant
// id, before either is trusted.
func TestDecodeDeliveryRoutesOnType(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)

	opened := `{"id":"evt_1","type":"dispute.opened","created_at":"2026-03-01T12:00:00Z",
		"data":{"dispute_id":"dsp_1","merchant_id":"mrc_1","transaction_id":"txn_1",
		"kind":"alert","card_network":"visa","reason_code":"10.4","amount_minor":4999,
		"currency":"USD","opened_at":"2026-03-01T11:00:00Z","respond_by":"2026-03-03T12:00:00Z"}}`
	resolved := `{"id":"evt_2","type":"dispute.resolved","created_at":"2026-03-01T12:00:00Z",
		"data":{"dispute_id":"dsp_1","merchant_id":"mrc_1","outcome":"won",
		"decided_at":"2026-03-01T12:00:00Z","note":"compelling evidence"}}`

	t.Run("dispute.opened", func(t *testing.T) {
		t.Parallel()
		event, err := decodeDelivery([]byte(opened), now)
		if err != nil {
			t.Fatalf("decodeDelivery: %v", err)
		}
		if _, ok := event.(DisputeWebhook); !ok {
			t.Fatalf("decoded %T, want DisputeWebhook", event)
		}
		if event.eventID() != "evt_1" || event.merchantID() != "mrc_1" {
			t.Errorf("eventID %q, merchantID %q", event.eventID(), event.merchantID())
		}
	})

	t.Run("dispute.resolved", func(t *testing.T) {
		t.Parallel()
		event, err := decodeDelivery([]byte(resolved), now)
		if err != nil {
			t.Fatalf("decodeDelivery: %v", err)
		}
		if _, ok := event.(RulingWebhook); !ok {
			t.Fatalf("decoded %T, want RulingWebhook", event)
		}
		if event.eventID() != "evt_2" {
			t.Errorf("eventID = %q", event.eventID())
		}
	})

	// That a ruling body cannot be read as a dispute is
	// TestTheDecodersDoNotAcceptEachOthersBodies, one level down.

	t.Run("a type nothing handles", func(t *testing.T) {
		t.Parallel()
		_, err := decodeDelivery([]byte(`{"id":"evt_3","type":"dispute.exploded"}`), now)
		if err == nil || !strings.Contains(err.Error(), "dispute.exploded") {
			t.Errorf("err = %v, want the type it sent named back", err)
		}
	})

	t.Run("a deadline already past", func(t *testing.T) {
		t.Parallel()
		// Validate reads the clock the handler passes in, which is what makes
		// the Now hook worth having.
		_, err := decodeDelivery([]byte(opened), now.Add(72*time.Hour))
		if !errors.Is(err, ErrInvalidEvent) {
			t.Errorf("err = %v, want ErrInvalidEvent", err)
		}
	})
}
