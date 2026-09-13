package agent

import (
	"context"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"
)

// A base rate is what happened to disputes like this one, as a count.
//
// It exists because precedent retrieval needs text to match on and only 15% of
// open chargebacks carry a cardholder claim - so the feature built to inform a
// draft was unavailable for six disputes in seven. A base rate needs no text.
// Merchant and reason code are enough, and every dispute has both.
//
// It is a different kind of information from a precedent, and the difference
// matters when reading it. A precedent is an example: this is how an argument
// like yours was made and how it went. A base rate is a population: this is how
// often arguments like yours succeed here. The first helps decide HOW to write;
// the second helps decide WHETHER there is a case.
type BaseRate struct {
	// Scope is what the numbers cover: "merchant+reason" or "merchant".
	Scope      string `json:"scope"`
	ReasonCode string `json:"reason_code,omitempty"`

	Won  int `json:"won"`
	Lost int `json:"lost"`

	// Expired is not an outcome. A dispute whose deadline passed unattended was
	// never argued, so it says nothing about whether this kind of case is
	// winnable - but it says a great deal about the merchant, and burying it
	// inside a loss rate would hide the one number that is somebody's fault.
	Expired int `json:"expired"`
}

// Settled is how many disputes were actually argued to an outcome.
func (b BaseRate) Settled() int { return b.Won + b.Lost }

// minBaseRateSample is where a proportion starts meaning anything.
//
// Ten is not a statistical threshold, it is an honesty one. Below it the
// interval around any rate is wider than the range of answers somebody would
// act on differently, so reporting "67% win" from three disputes is reporting
// noise with a percent sign. Under ten, the counts go out and the rate does not.
const minBaseRateSample = 10

// Reliable reports whether the sample is large enough for WinRate to mean anything.
func (b BaseRate) Reliable() bool { return b.Settled() >= minBaseRateSample }

// WinRate is only meaningful when Reliable.
func (b BaseRate) WinRate() float64 {
	if b.Settled() == 0 {
		return 0
	}
	return float64(b.Won) / float64(b.Settled())
}

// baseRates returns the reason-specific rate and the merchant-wide one.
//
// Two scopes because the narrow one is the better answer and the wide one is
// the one that exists. A merchant with four settled disputes under this reason
// code has nothing to say about it, and falling back to the whole merchant is
// weaker but not noise.
func baseRates(ctx context.Context, pool *pgxpool.Pool, merchant, reasonCode, kind string) ([]BaseRate, error) {
	rows, err := pool.Query(ctx, `
		SELECT scope, won, lost, expired FROM (
		  SELECT 'merchant+reason' AS scope, 1 AS ord,
		         count(*) FILTER (WHERE d.state = 'won')     AS won,
		         count(*) FILTER (WHERE d.state = 'lost')    AS lost,
		         count(*) FILTER (WHERE d.state = 'expired') AS expired
		    FROM disputes  d
		    JOIN merchants m ON m.id = d.merchant_id
		   WHERE m.external_id = $1 AND d.kind = $3 AND d.reason_code = $2
		  UNION ALL
		  SELECT 'merchant', 2,
		         count(*) FILTER (WHERE d.state = 'won'),
		         count(*) FILTER (WHERE d.state = 'lost'),
		         count(*) FILTER (WHERE d.state = 'expired')
		    FROM disputes  d
		    JOIN merchants m ON m.id = d.merchant_id
		   WHERE m.external_id = $1 AND d.kind = $3
		) x ORDER BY ord`, merchant, reasonCode, kind)
	if err != nil {
		return nil, fmt.Errorf("base rates: %w", err)
	}
	defer rows.Close()

	var out []BaseRate
	for rows.Next() {
		var b BaseRate
		if err := rows.Scan(&b.Scope, &b.Won, &b.Lost, &b.Expired); err != nil {
			return nil, fmt.Errorf("scan base rate: %w", err)
		}
		if b.Scope == "merchant+reason" {
			b.ReasonCode = reasonCode
		}
		// A scope with nothing settled and nothing expired has no content.
		if b.Settled() > 0 || b.Expired > 0 {
			out = append(out, b)
		}
	}
	return out, rows.Err()
}

// renderBaseRates writes them out with the sample size attached to every claim.
//
// The danger this block introduces is a model reasoning from the population
// instead of from the record: "only 12% of these win, so recommend
// insufficient_evidence" is the statistic deciding a case it knows nothing
// about. The heading says so, and it says it before the numbers rather than
// after.
func (f Facts) renderBaseRates() string {
	if len(f.BaseRates) == 0 {
		return "\nBASE RATES\nNone: this merchant has no settled disputes to compare against.\n"
	}

	var b strings.Builder
	b.WriteString("\nBASE RATES\n")
	b.WriteString("How disputes like this one have gone at this merchant. This describes a population and says nothing about whether THIS dispute is winnable: a low rate is not a reason to decline, and a high one is not evidence. Judge the case on the record above.\n")

	for _, rate := range f.BaseRates {
		scope := "all chargebacks at this merchant"
		if rate.Scope == "merchant+reason" {
			scope = "reason code " + rate.ReasonCode + " at this merchant"
		}

		fmt.Fprintf(&b, "- %s: %d won, %d lost", scope, rate.Won, rate.Lost)
		if rate.Reliable() {
			fmt.Fprintf(&b, " (%.0f%% of %d settled)", 100*rate.WinRate(), rate.Settled())
		} else if rate.Settled() > 0 {
			fmt.Fprintf(&b, " (%d settled - too few to be a rate)", rate.Settled())
		}
		if rate.Expired > 0 {
			// Reported, and named for what it is. An expired dispute was never
			// argued, so it belongs to neither column.
			fmt.Fprintf(&b, "; %d more expired unattended and were never argued", rate.Expired)
		}
		b.WriteString("\n")
	}
	return b.String()
}
