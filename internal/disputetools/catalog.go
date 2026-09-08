package disputetools

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/google/jsonschema-go/jsonschema"
)

// ErrInvalidArguments marks a tool call whose arguments could not be decoded:
// a misspelled field, a string where a number belongs, JSON that does not
// parse. It is deliberately separate from the errors a tool returns after its
// arguments were understood, because a caller has to tell "you asked wrong"
// from "the system is broken" - the first is worth answering, the second is
// not.
var ErrInvalidArguments = errors.New("invalid arguments")

// Definition is one tool, described in the only terms both transports agree
// on: a name, a description, a JSON Schema for the input, and a function that
// takes raw JSON and returns a value ready to be marshalled back.
//
// The MCP SDK and the Messages API both want exactly this and disagree only
// about the field names around it.
type Definition struct {
	Name        string
	Description string
	InputSchema json.RawMessage
	Invoke      func(ctx context.Context, input json.RawMessage) (any, error)
}

// Catalog is the four tools, in the order a caller most often needs them.
func (s *Set) Catalog() []Definition {
	return []Definition{
		{
			Name:        "list_disputes",
			Description: DescListDisputes,
			InputSchema: schemaFor[ListDisputesInput](),
			Invoke:      bind(s.ListDisputes),
		},
		{
			Name:        "get_dispute",
			Description: DescGetDispute,
			InputSchema: schemaFor[GetDisputeInput](),
			Invoke:      bind(s.GetDispute),
		},
		{
			Name:        "get_customer_history",
			Description: DescCustomerHistory,
			InputSchema: schemaFor[CustomerHistoryInput](),
			Invoke:      bind(s.CustomerHistory),
		},
		{
			Name:        "queue_summary",
			Description: DescQueueSummary,
			InputSchema: schemaFor[QueueSummaryInput](),
			Invoke:      bind(s.QueueSummary),
		},
	}
}

// bind turns a typed handler into one that speaks raw JSON, which is what both
// transports actually deliver.
//
// Decoding is strict. A model that invents a parameter - filtering by
// "merchant_id" when the field is "merchant" - gets an error it can read and
// correct. Ignoring the unknown field silently would hand back every merchant's
// disputes while the model believes its filter applied, and nothing downstream
// would ever notice the difference.
func bind[In, Out any](fn func(context.Context, In) (Out, error)) func(context.Context, json.RawMessage) (any, error) {
	return func(ctx context.Context, raw json.RawMessage) (any, error) {
		var in In
		if len(bytes.TrimSpace(raw)) > 0 {
			dec := json.NewDecoder(bytes.NewReader(raw))
			dec.DisallowUnknownFields()
			if err := dec.Decode(&in); err != nil {
				return nil, fmt.Errorf("%w: %s", ErrInvalidArguments, err)
			}
		}
		out, err := fn(ctx, in)
		if err != nil {
			return nil, err
		}
		return out, nil
	}
}

// schemaFor derives the input schema from the Go struct, using the same
// library and the same jsonschema struct tags the MCP SDK reads. The
// description a model sees is the tag next to the field it describes, which is
// the only place it cannot drift away from.
//
// A failure here is a malformed struct definition, not a runtime condition, so
// it panics: the alternative is a process that starts successfully and then
// offers a model tools it cannot describe.
func schemaFor[In any]() json.RawMessage {
	schema, err := jsonschema.For[In](nil)
	if err != nil {
		panic(fmt.Sprintf("disputetools: cannot derive schema for %T: %v", *new(In), err))
	}
	encoded, err := json.Marshal(schema)
	if err != nil {
		panic(fmt.Sprintf("disputetools: cannot encode schema for %T: %v", *new(In), err))
	}
	return encoded
}
