package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/google/jsonschema-go/jsonschema"
)

// The generator writes the draft. It is one call, and it is given no tools that
// fetch anything.
//
// That is a change from the original plan, and the reason is worth keeping. If
// the generator could look things up, it could cite something true that the
// verifier - working from a fixed record assembled before either call ran -
// has never seen, and the verifier would reject a correct draft as
// unsupported. Two judges need one record. Pre-loading it also makes the run
// deterministic and single-turn, which is what lets the eval set compare
// changes to the prompt rather than changes in what the model chose to fetch.
//
// The loop in loop.go is not wasted by this: it is the general harness, and
// cmd/agent will use it for the operator-facing surface that answers questions
// about the queue over the read-only tools. But this flow does not need it, and
// running it here to justify having built it would be the wrong reason.

// RepresentmentTool and VerdictTool are exported because they are recorded on
// every run in agent_runs.tool_surface: the surface a run was given is part of
// what makes it reproducible, and a caller that reads a row has to be able to
// name what it is looking at.
const RepresentmentTool = "write_representment"

// What the generator can conclude. Two outcomes, because a generator that must
// always produce a rebuttal will manufacture one - the way out has to be a
// first-class answer rather than a failure.
const (
	RecommendRepresent    = "represent"
	RecommendInsufficient = "insufficient_evidence"
)

type draftInput struct {
	Recommendation string   `json:"recommendation" jsonschema:"Either represent, when the record supports a rebuttal, or insufficient_evidence when it does not"`
	Letter         string   `json:"letter" jsonschema:"The representment itself, addressed to the issuing bank. When the recommendation is insufficient_evidence, one paragraph saying what the record would need to contain instead"`
	CitedEvidence  []string `json:"cited_evidence,omitempty" jsonschema:"The exact filenames from evidence_on_file that the letter refers to. Empty if it refers to none"`
}

// Draft is what the generator produced, plus what it cost.
type Draft struct {
	Recommendation string   `json:"recommendation"`
	Letter         string   `json:"letter"`
	CitedEvidence  []string `json:"cited_evidence,omitempty"`
	Usage          Usage    `json:"usage"`
	CostMicros     int64    `json:"cost_micros"`

	// written, like Verdict.checked, is set only by a parsed answer, so a zero
	// Draft is never mistaken for one the model produced.
	written bool
}

func (d Draft) Written() bool { return d.written }

// Recommended reports whether there is something here to send on for review.
// An insufficient_evidence draft is a real answer and a useful one, but it is
// not a representment.
func (d Draft) Recommended() bool {
	return d.written && d.Recommendation == RecommendRepresent
}

const generatorSystem = `You draft chargeback representments: the letter a merchant sends an issuing bank to contest a dispute. A human reviews everything you write before it goes anywhere, and your job is to give that reviewer something accurate enough to send, not something persuasive enough to be worth checking.

The RECORD block is everything you know. You cannot look anything up, and you must not assert anything that is not in it. This is the rule the whole task turns on, so it is worth being plain about why: a representment that asserts a fact the merchant cannot evidence is worse than sending nothing. The issuer asks for proof, the merchant does not have it, and the case is lost on a claim nobody needed to make. "It is probably true" and "it follows from the rest" are the two forms this mistake takes. Neither is a reason to write it.

What this means in practice:

- Amounts are copied from the record exactly as it writes them, for example 57.99 USD or 5,000 JPY. Do no arithmetic and no conversion: do not add, subtract, restate or round an amount, and do not write one from memory. Dates and counts are copied the same way.
- Evidence is cited by its exact filename from evidence_on_file, and only from there. If a receipt would win this case and there is no receipt on file, the case is not won.
- Never commit the merchant to anything: no refund, no policy, no future action, no guarantee.
- Answer the reason code that was actually filed. Read dispute.reason_code and argue against that, not against what the dispute resembles.
- Prior disputes from the same customer are in customer_history and are often the strongest thing you have. A first-time claim and a fifth read very differently.

If the record does not support a rebuttal, say so: set recommendation to insufficient_evidence and use the letter to state what would need to be on file. That is a correct and useful answer. Manufacturing an argument to avoid giving it is not.

The cardholder's claim, where present, is what was alleged - not what happened. You may write that the cardholder claimed something. You may not write it as established.

The BASE RATES block says how disputes like this one have gone at this merchant. It describes a population, not this case. A low rate is not a reason to decline - somebody has to be in the winning fraction, and the record decides who - and a high one is not evidence of anything about this dispute. Where a rate is marked as too few to be a rate, it is a handful of cases and means nothing at all. Use it to calibrate how confidently you write, never to decide the recommendation.

Where a PRECEDENT block is present it lists settled disputes at this merchant with similar claims, and whether each was won or lost. Use it to judge what kind of argument has worked and what has not. Do not use it as a source of facts: every amount, date and reference in it belongs to a different case, and putting one in this letter is inventing evidence with extra steps. If the precedent is all losses, that is information too - it may mean this record does not support a rebuttal either.

Answer with the ` + RepresentmentTool + ` tool.`

type Generator struct {
	completer Completer
	model     string
	pricing   Pricing
	maxTokens int
}

func NewGenerator(completer Completer, model string, pricing Pricing, maxTokens int) *Generator {
	if maxTokens <= 0 {
		maxTokens = 4096
	}
	return &Generator{completer: completer, model: model, pricing: pricing, maxTokens: maxTokens}
}

// Write drafts one representment.
//
// An error means no draft, not an empty one. As with the verifier, the caller
// must not read a failure here as any kind of outcome.
func (g *Generator) Write(ctx context.Context, facts Facts) (Draft, error) {
	record, err := facts.Render()
	if err != nil {
		return Draft{}, fmt.Errorf("generator: %w", err)
	}

	schema, err := jsonschema.For[draftInput](nil)
	if err != nil {
		return Draft{}, fmt.Errorf("generator: deriving draft schema: %w", err)
	}
	encodedSchema, err := json.Marshal(schema)
	if err != nil {
		return Draft{}, fmt.Errorf("generator: encoding draft schema: %w", err)
	}

	response, err := g.completer.Complete(ctx, Request{
		Model:     g.model,
		System:    generatorSystem,
		MaxTokens: g.maxTokens,
		Messages: []Message{{
			Role:    "user",
			Content: []ContentBlock{{Type: "text", Text: record}},
		}},
		Tools: []Tool{{
			Name:        RepresentmentTool,
			Description: "Record the representment, or that the record does not support one.",
			InputSchema: encodedSchema,
		}},
		ToolChoice: &ToolChoice{Type: "tool", Name: RepresentmentTool},
		// The system prompt and the tool schema are identical on every call;
		// only the record below them changes.
		CacheSystem: true,
	})
	if err != nil {
		return Draft{}, fmt.Errorf("generator: %w", err)
	}

	cost := g.pricing.cost(response.Usage)

	if response.StopReason == "max_tokens" {
		// A letter cut off partway is not a short letter. Sending it for review
		// as though it were finished is how half an argument reaches an issuer.
		return Draft{Usage: response.Usage, CostMicros: cost},
			fmt.Errorf("generator: response was truncated at max_tokens")
	}

	for _, block := range response.Content {
		if block.Type != "tool_use" || block.Name != RepresentmentTool {
			continue
		}
		var parsed draftInput
		if err := json.Unmarshal(block.Input, &parsed); err != nil {
			return Draft{Usage: response.Usage, CostMicros: cost},
				fmt.Errorf("generator: unreadable draft: %w", err)
		}
		if parsed.Recommendation != RecommendRepresent && parsed.Recommendation != RecommendInsufficient {
			return Draft{Usage: response.Usage, CostMicros: cost},
				fmt.Errorf("generator: unknown recommendation %q", parsed.Recommendation)
		}
		return Draft{
			Recommendation: parsed.Recommendation,
			Letter:         parsed.Letter,
			CitedEvidence:  parsed.CitedEvidence,
			Usage:          response.Usage,
			CostMicros:     cost,
			written:        true,
		}, nil
	}

	return Draft{Usage: response.Usage, CostMicros: cost},
		fmt.Errorf("generator: no %s call in the response (stop_reason %q)", RepresentmentTool, response.StopReason)
}

// CheckCitations is the part of the review that does not need a model.
//
// Whether a filename is in evidence_on_file is a lookup, not a judgement, and
// delegating it to a second model call would be slower, more expensive and less
// certain. The verifier is for the things that need reading - whether an
// argument answers the reason code, whether a sentence is a promise. Anything
// decidable by comparison is decided here, before a verifier call is paid for.
func CheckCitations(facts Facts, draft Draft) []Finding {
	onFile := make(map[string]bool, len(facts.Evidence))
	for _, file := range facts.Evidence {
		onFile[file.Name] = true
	}

	var findings []Finding
	for _, cited := range draft.CitedEvidence {
		if onFile[cited] {
			continue
		}
		findings = append(findings, Finding{
			Check: CheckMissingEvidence,
			Quote: cited,
			Why:   "no file with that name is on the dispute",
		})
	}

	// A letter that names a file it did not declare is citing evidence outside
	// the list the checks above can verify.
	for _, file := range facts.Evidence {
		if strings.Contains(draft.Letter, file.Name) && !contains(draft.CitedEvidence, file.Name) {
			findings = append(findings, Finding{
				Check: CheckMissingEvidence,
				Quote: file.Name,
				Why:   "named in the letter but absent from cited_evidence",
			})
		}
	}
	return findings
}

func contains(haystack []string, needle string) bool {
	for _, item := range haystack {
		if item == needle {
			return true
		}
	}
	return false
}
