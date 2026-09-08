package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/bedrockruntime"
)

// Bedrock is the Completer that actually costs money.
//
// It is the only file in this package that knows a model is reached over a
// network. Everything else - the loop, the generator, the verifier - is written
// against the interface, which is what lets the tests and the eval set run
// against a script for free. That seam is the same one internal/outbox has for
// its Publisher, and it earns its keep the same way.
//
// Credentials are never configured here. On ECS the task role supplies them,
// which is the whole argument for Bedrock over a direct API key: there is no
// secret to store, rotate, or leak, and access is revoked by editing an IAM
// policy rather than by finding every copy of a string.
type Bedrock struct {
	client  *bedrockruntime.Client
	modelID string
}

func NewBedrock(client *bedrockruntime.Client, modelID string) (*Bedrock, error) {
	if modelID == "" {
		// Refused rather than defaulted. Bedrock model ids are region-scoped
		// and versioned - a guess here fails deep inside the SDK with a message
		// about an invalid identifier, long after the useful place to say so.
		return nil, errors.New("agent: BEDROCK_MODEL_ID is required " +
			"(list them with: aws bedrock list-foundation-models --query 'modelSummaries[].modelId')")
	}
	return &Bedrock{client: client, modelID: modelID}, nil
}

// bedrockBody is the request as InvokeModel wants it.
//
// Written out rather than marshalling Request directly, because Bedrock differs
// from the direct API in exactly two ways and both belong here rather than
// leaking into the types the rest of the package uses: the model is named in
// the API call instead of the body, and the body carries anthropic_version.
type bedrockBody struct {
	AnthropicVersion string      `json:"anthropic_version"`
	MaxTokens        int         `json:"max_tokens"`
	System           string      `json:"system,omitempty"`
	Messages         []Message   `json:"messages"`
	Tools            []Tool      `json:"tools,omitempty"`
	ToolChoice       *ToolChoice `json:"tool_choice,omitempty"`
	Temperature      *float64    `json:"temperature,omitempty"`
}

const bedrockAnthropicVersion = "bedrock-2023-05-31"

func (b *Bedrock) Complete(ctx context.Context, req Request) (Response, error) {
	body, err := json.Marshal(bedrockBody{
		AnthropicVersion: bedrockAnthropicVersion,
		MaxTokens:        req.MaxTokens,
		System:           req.System,
		Messages:         req.Messages,
		Tools:            req.Tools,
		ToolChoice:       req.ToolChoice,
		Temperature:      req.Temperature,
	})
	if err != nil {
		return Response{}, fmt.Errorf("bedrock: encoding request: %w", err)
	}

	out, err := b.client.InvokeModel(ctx, &bedrockruntime.InvokeModelInput{
		ModelId:     aws.String(b.modelID),
		ContentType: aws.String("application/json"),
		Accept:      aws.String("application/json"),
		Body:        body,
	})
	if err != nil {
		return Response{}, fmt.Errorf("bedrock: invoke %s: %w", b.modelID, err)
	}

	var response Response
	if err := json.Unmarshal(out.Body, &response); err != nil {
		return Response{}, fmt.Errorf("bedrock: decoding response: %w", err)
	}
	if response.StopReason == "" {
		// Every real response carries one, and the callers branch on it. An
		// empty value would fall through to the default case and be recorded
		// as an unexpected stop reason, which hides the actual problem.
		return Response{}, fmt.Errorf("bedrock: response carried no stop_reason")
	}
	return response, nil
}
