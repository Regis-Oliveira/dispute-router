package worker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	"github.com/aws/aws-sdk-go-v2/service/sqs/types"
)

// ErrUnhandled marks a message this consumer cannot act on. Returning it leaves
// the message to be retried and, after enough attempts, moved to the
// dead-letter queue by the redrive policy - rather than being deleted quietly.
var ErrUnhandled = errors.New("message cannot be handled")

// envelope is what the outbox relay puts on the queue.
type envelope struct {
	OutboxID      int64           `json:"outbox_id"`
	AggregateType string          `json:"aggregate_type"`
	AggregateID   int64           `json:"aggregate_id"`
	EventType     string          `json:"event_type"`
	Payload       json.RawMessage `json:"payload"`
}

type disputeReceived struct {
	DisputeID  int64     `json:"dispute_id"`
	DeadlineAt time.Time `json:"deadline_at"`
}

// Consumer turns queue messages into scheduled work.
//
// Its only job is to put a new dispute into the deadline index the moment it
// exists. Without it a dispute waits for the next reconcile pass - fine for a
// chargeback with three weeks on the clock, useless for an alert with an hour.
//
// The reconcile pass stays: this is the fast path, that is the guarantee.
type Consumer struct {
	opts ConsumerOptions
}

// ConsumerOptions configures a Consumer; NewConsumer fills in MaxMessages and
// WaitTime when they are zero.
type ConsumerOptions struct {
	Client    *sqs.Client
	QueueURL  string
	Deadlines *Deadlines
	Logger    *slog.Logger

	// MaxMessages per receive, 1-10 (the SQS limit).
	MaxMessages int32
	// WaitTime turns each receive into a long poll. Zero would busy-poll an
	// empty queue and bill for it; twenty seconds is the SQS maximum and means
	// one request per twenty seconds when there is nothing to do, and an
	// immediate return when there is.
	WaitTime int32
}

// NewConsumer builds a Consumer, defaulting MaxMessages to 10 and WaitTime to
// 20 seconds. It is the only way to build one: a Consumer literal with a zero
// WaitTime would busy-poll, so the fields are not exported.
func NewConsumer(opts ConsumerOptions) *Consumer {
	if opts.MaxMessages < 1 {
		opts.MaxMessages = 10
	}
	if opts.WaitTime < 1 {
		opts.WaitTime = 20
	}
	return &Consumer{opts: opts}
}

// Run receives and handles messages until ctx is cancelled.
func (c *Consumer) Run(ctx context.Context) error {
	c.opts.Logger.InfoContext(ctx, "sqs consumer starting", "queue", c.opts.QueueURL)

	for {
		if ctx.Err() != nil {
			c.opts.Logger.InfoContext(ctx, "sqs consumer stopping")
			return nil
		}

		out, err := c.opts.Client.ReceiveMessage(ctx, &sqs.ReceiveMessageInput{
			QueueUrl:              aws.String(c.opts.QueueURL),
			MaxNumberOfMessages:   c.opts.MaxMessages,
			WaitTimeSeconds:       c.opts.WaitTime,
			MessageAttributeNames: []string{"All"},
			// ApproximateReceiveCount is how many times this message has been
			// delivered. It is the only way to tell a first attempt from a
			// fifth, and it is what makes a poison message visible before the
			// redrive policy quietly removes it.
			MessageSystemAttributeNames: []types.MessageSystemAttributeName{
				types.MessageSystemAttributeNameApproximateReceiveCount,
			},
		})
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			c.opts.Logger.ErrorContext(ctx, "sqs receive failed", "error", err)
			// Back off rather than hammering a failing endpoint.
			select {
			case <-ctx.Done():
				return nil
			case <-time.After(2 * time.Second):
			}
			continue
		}

		for _, msg := range out.Messages {
			c.handle(ctx, msg)
		}
	}
}

func (c *Consumer) handle(ctx context.Context, msg types.Message) {
	receives := msg.Attributes[string(types.MessageSystemAttributeNameApproximateReceiveCount)]

	if err := c.process(ctx, msg); err != nil {
		// Not deleted. The message becomes visible again when its visibility
		// timeout lapses, and after maxReceiveCount attempts the redrive policy
		// moves it to the dead-letter queue. Deleting it here to keep the logs
		// quiet is how a lost message becomes a mystery.
		c.opts.Logger.ErrorContext(ctx, "message not handled; leaving it for redelivery",
			"error", err, "receive_count", receives)
		return
	}

	// Deleting is the acknowledgement, and it happens only after the work is
	// durably done. A crash before this point means redelivery, which is why
	// scheduling has to be idempotent - and it is: ZADD on an id already
	// present moves it rather than duplicating it.
	if _, err := c.opts.Client.DeleteMessage(ctx, &sqs.DeleteMessageInput{
		QueueUrl:      aws.String(c.opts.QueueURL),
		ReceiptHandle: msg.ReceiptHandle,
	}); err != nil {
		c.opts.Logger.ErrorContext(ctx, "delete failed; the message will be redelivered", "error", err)
	}
}

func (c *Consumer) process(ctx context.Context, msg types.Message) error {
	if msg.Body == nil {
		return fmt.Errorf("%w: empty body", ErrUnhandled)
	}

	var env envelope
	if err := json.Unmarshal([]byte(*msg.Body), &env); err != nil {
		return fmt.Errorf("%w: %s", ErrUnhandled, err)
	}

	// Only the arrival of a new dispute needs scheduling. The worker's own
	// outcome events come through here too and are acknowledged without work,
	// because a queue nobody drains is a queue that fills up.
	if env.EventType != "dispute.received" {
		return nil
	}

	var payload disputeReceived
	if err := json.Unmarshal(env.Payload, &payload); err != nil {
		return fmt.Errorf("%w: payload: %s", ErrUnhandled, err)
	}
	if payload.DisputeID == 0 || payload.DeadlineAt.IsZero() {
		return fmt.Errorf("%w: payload is missing dispute_id or deadline_at", ErrUnhandled)
	}

	if err := c.opts.Deadlines.Schedule(ctx, payload.DisputeID, payload.DeadlineAt); err != nil {
		// A transient Redis failure. Worth retrying, so it is not ErrUnhandled.
		return err
	}

	c.opts.Logger.InfoContext(ctx, "scheduled from queue",
		"dispute_id", payload.DisputeID,
		"deadline_at", payload.DeadlineAt,
		"outbox_id", env.OutboxID)
	return nil
}
