package agent

import (
	"bytes"
	"log/slog"
	"os"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/regisoliveira/dispute-router/internal/llm"
	"github.com/regisoliveira/dispute-router/internal/llm/llmtest"
)

// Ten settled disputes is where a proportion starts meaning anything. Below it
// the counts go out and the rate does not - "67% win" from three disputes is
// noise with a percent sign on it.
func TestASmallSampleIsNotARate(t *testing.T) {
	t.Parallel()

	small := Facts{BaseRates: []BaseRate{
		{Scope: "merchant+reason", ReasonCode: "10.4", Won: 2, Lost: 1},
	}}
	rendered := small.renderBaseRates()
	if !strings.Contains(rendered, "too few to be a rate") {
		t.Errorf("a 3-dispute sample was reported as a rate:\n%s", rendered)
	}
	if strings.Contains(rendered, "%") && !strings.Contains(rendered, "too few") {
		t.Error("a percentage escaped from a sample too small to carry one")
	}

	big := Facts{BaseRates: []BaseRate{
		{Scope: "merchant+reason", ReasonCode: "10.4", Won: 4, Lost: 6},
	}}
	if !strings.Contains(big.renderBaseRates(), "40% of 10 settled") {
		t.Errorf("a 10-dispute sample did not produce a rate:\n%s", big.renderBaseRates())
	}
}

// An expired dispute was never argued. Folding it into losses would say this
// kind of case is unwinnable when what happened is that nobody tried.
func TestExpiredIsReportedSeparately(t *testing.T) {
	t.Parallel()

	facts := Facts{BaseRates: []BaseRate{
		{Scope: "merchant", Won: 10, Lost: 10, Expired: 40},
	}}
	rendered := facts.renderBaseRates()

	if !strings.Contains(rendered, "50% of 20 settled") {
		t.Errorf("expired disputes were counted into the rate:\n%s", rendered)
	}
	if !strings.Contains(rendered, "40 more expired unattended") {
		t.Error("the expired count was dropped; it is the one number that is somebody's fault")
	}
}

// The failure this block could cause: a model declining a winnable case because
// the population loses more often than it wins.
func TestTheBlockSaysItIsNotAboutThisDispute(t *testing.T) {
	t.Parallel()

	facts := Facts{BaseRates: []BaseRate{{Scope: "merchant", Won: 2, Lost: 98}}}
	rendered := facts.renderBaseRates()

	if !strings.Contains(rendered, "says nothing about whether THIS dispute is winnable") {
		t.Error("the block does not separate the population from the case")
	}
	if !strings.Contains(rendered, "a low rate is not a reason to decline") {
		t.Error("the block does not guard against the statistic deciding the case")
	}
}

func TestAMerchantWithNoHistorySaysSo(t *testing.T) {
	t.Parallel()

	rendered := Facts{}.renderBaseRates()
	if !strings.Contains(rendered, "no settled disputes to compare against") {
		t.Error("an absent base rate was passed over in silence")
	}
}

// Both calls see it, for the same reason precedent does: two judges working
// from different records disagree about facts neither of them can check.
func TestBaseRatesReachBothCalls(t *testing.T) {
	t.Parallel()

	facts := Facts{BaseRates: []BaseRate{
		{Scope: "merchant+reason", ReasonCode: "13.1", Won: 12, Lost: 18},
	}}

	gen := &llmtest.ScriptedCompleter{Responses: []llm.Response{draftResponseFor(t, RecommendRepresent, "x")}}
	if _, err := NewGenerator(gen, "t", llm.Pricing{}, 4096).Write(t.Context(), facts); err != nil {
		t.Fatalf("generator: %v", err)
	}
	ver := &llmtest.ScriptedCompleter{Responses: []llm.Response{verdictPass(t)}}
	if _, err := NewVerifier(ver, "t", llm.Pricing{}, 2048).Check(t.Context(), facts, "x"); err != nil {
		t.Fatalf("verifier: %v", err)
	}

	for name, script := range map[string]*llmtest.ScriptedCompleter{"generator": gen, "verifier": ver} {
		if !strings.Contains(script.Requests[0].Messages[0].Content[0].Text, "40% of 30 settled") {
			t.Errorf("the %s never saw the base rate", name)
		}
	}
	if !strings.Contains(ver.Requests[0].System, "reasoned from a statistic") {
		t.Error("the verifier was not told to catch a case declined on the population")
	}
}

// Every dispute has a merchant and a reason code, which is the whole point:
// precedent covers about one in seven, this covers all of them.
func TestBaseRatesExistForAClaimlessDispute(t *testing.T) {
	if os.Getenv("DATABASE_URL") == "" {
		t.Skip("DATABASE_URL not set")
	}
	store, pool := liveStore(t)

	var id int64
	err := pool.QueryRow(t.Context(), `
		SELECT id FROM disputes
		 WHERE kind = 'chargeback' AND cardholder_claim = ''
		   AND state = 'received' ORDER BY id LIMIT 1`).Scan(&id)
	if err != nil {
		t.Skipf("no claimless open chargeback: %v", err)
	}

	facts, err := factSource(t, store, fakeEvidence{}, FactSourceOptions{
		Precedent: NewRetriever(pool, nil, 3),
		BaseRates: pool,
	}).For(t.Context(), id)
	if err != nil {
		t.Fatalf("For: %v", err)
	}

	if len(facts.Precedents) != 0 {
		t.Log("this dispute happens to have precedent; the point still holds")
	}
	if len(facts.BaseRates) == 0 {
		t.Fatal("a dispute with no claim got no base rate either; the gap is not closed")
	}
	t.Logf("dispute %d: %d precedent(s), %d base rate scope(s)", id, len(facts.Precedents), len(facts.BaseRates))
}

// A base-rate query that fails must cost the draft its population numbers and
// nothing else - and it must say so.
//
// The silence is the part under test. Retrieval records its failure in the
// trace, where a reader of agent_runs finds it; base rates have no such field,
// so the log line is the only thing between a broken aggregate and every
// subsequent draft quietly losing a paragraph of context.
func TestABrokenBaseRateQueryCostsTheRatesAndSaysSo(t *testing.T) {
	if os.Getenv("DATABASE_URL") == "" {
		t.Skip("DATABASE_URL not set")
	}
	store, _ := liveStore(t)
	id := someDisputeID(t, store)

	// A pool that is closed rather than misconfigured: it fails at query time,
	// which is where a pool that has lost its database fails, and not at
	// construction, where nothing under test would ever see it.
	broken, err := pgxpool.New(t.Context(), os.Getenv("DATABASE_URL"))
	if err != nil {
		t.Fatalf("second pool: %v", err)
	}
	broken.Close()

	var logged bytes.Buffer
	source := factSource(t, store, fakeEvidence{}, FactSourceOptions{
		BaseRates: broken,
		Logger:    slog.New(slog.NewTextHandler(&logged, &slog.HandlerOptions{Level: slog.LevelWarn})),
	})

	facts, err := source.For(t.Context(), id)
	if err != nil {
		t.Fatalf("a failed base-rate query failed the whole record: %v", err)
	}
	if len(facts.BaseRates) != 0 {
		t.Fatalf("got %d base rates from a closed pool", len(facts.BaseRates))
	}
	if facts.Dispute.ID != id {
		t.Fatalf("the record lost its dispute: got %d, want %d", facts.Dispute.ID, id)
	}

	line := logged.String()
	if !strings.Contains(line, "drafting without base rates") {
		t.Fatalf("the failure was swallowed with no log line; got %q", line)
	}
	// The identifiers that make the line actionable, and none from the
	// cardholder's side of the record.
	for _, want := range []string{"dispute=", "merchant=", "reason_code="} {
		if !strings.Contains(line, want) {
			t.Errorf("log line carries no %s: %q", want, line)
		}
	}
}
