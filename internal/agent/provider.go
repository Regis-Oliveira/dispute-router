package agent

import (
	"context"
	"fmt"

	"github.com/aws/aws-sdk-go-v2/service/bedrockruntime"
)

// Provider picks the Completer and the name recorded against every run.
//
// The model id goes into agent_runs.model and into the eval report, so it has
// to be the id that was actually called - not the provider, and not a label. A
// row that says "anthropic" cannot answer whether a change in pass rate came
// from a change of model.
type Provider struct {
	Kind            string
	AnthropicAPIKey string
	AnthropicModel  string
	BedrockModelID  string
	BedrockClient   *bedrockruntime.Client
}

func (p Provider) Build(_ context.Context) (Completer, string, error) {
	switch p.Kind {
	case "anthropic":
		completer, err := NewAnthropic(p.AnthropicAPIKey, p.AnthropicModel)
		if err != nil {
			return nil, "", err
		}
		return completer, p.AnthropicModel, nil

	case "bedrock":
		completer, err := NewBedrock(p.BedrockClient, p.BedrockModelID)
		if err != nil {
			return nil, "", err
		}
		return completer, p.BedrockModelID, nil

	default:
		return nil, "", fmt.Errorf("agent: unknown model provider %q", p.Kind)
	}
}
