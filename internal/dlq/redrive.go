// Package dlq inspects and drains the dead-letter queue.
//
// A dead-letter queue nobody can read is a bin. The point of one is the loop it
// makes possible: look at what failed, fix the cause, put the messages back.
// Without a way to do the last step, every message that lands there is lost -
// just more slowly and with more ceremony than dropping it would have been.
package dlq

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	"github.com/aws/aws-sdk-go-v2/service/sqs/types"
)

// redriveCountAttribute counts how many times a message has been put back.
//
// SQS's own ApproximateReceiveCount resets when a message is re-sent, so
// without carrying this ourselves a message can be replayed into the same
// failure forever and look new every time.
const redriveCountAttribute = "redrive_count"

type Message struct {
	ReceiptHandle string
	Body          string
	// Receives is how many times the queue has delivered this message.
	Receives int
	// Redrives is how many times an operator has replayed it.
	Redrives  int
	FirstSeen time.Time
}

// Summary is what a body says about itself, for the operator staring at it.
func (m Message) Summary() string {
	var envelope struct {
		OutboxID    int64  `json:"outbox_id"`
		EventType   string `json:"event_type"`
		AggregateID int64  `json:"aggregate_id"`
	}
	if err := json.Unmarshal([]byte(m.Body), &envelope); err != nil {
		return "unparseable body"
	}
	if envelope.EventType == "" {
		return "no event_type"
	}
	return fmt.Sprintf("%s aggregate=%d outbox=%d",
		envelope.EventType, envelope.AggregateID, envelope.OutboxID)
}

type Redriver struct {
	Client *sqs.Client
	// Source is the dead-letter queue; Target is where replayed messages go.
	Source string
	Target string
}

func (r *Redriver) Depth(ctx context.Context, queueURL string) (int, error) {
	out, err := r.Client.GetQueueAttributes(ctx, &sqs.GetQueueAttributesInput{
		QueueUrl: aws.String(queueURL),
		AttributeNames: []types.QueueAttributeName{
			types.QueueAttributeNameApproximateNumberOfMessages,
		},
	})
	if err != nil {
		return 0, fmt.Errorf("queue depth: %w", err)
	}
	return strconv.Atoi(out.Attributes[string(types.QueueAttributeNameApproximateNumberOfMessages)])
}

// receive pulls a batch.
//
// Note what visibilityTimeout 0 does *not* do here. The SDK writes this field
// only when it is non-zero:
//
//	if v.VisibilityTimeout != 0 { s.WriteInt32(...) }
//
// so passing 0 is indistinguishable from not passing it, and the queue's own
// default applies instead. A "peek" that thought it was leaving messages
// visible was reserving them for thirty seconds, which is why the command after
// it found an empty queue. Use release() to actually hand a message back.
func (r *Redriver) receive(ctx context.Context, limit int32, visibilityTimeout int32) ([]Message, error) {
	out, err := r.Client.ReceiveMessage(ctx, &sqs.ReceiveMessageInput{
		QueueUrl:              aws.String(r.Source),
		MaxNumberOfMessages:   min(limit, 10),
		VisibilityTimeout:     visibilityTimeout,
		WaitTimeSeconds:       1,
		MessageAttributeNames: []string{"All"},
		MessageSystemAttributeNames: []types.MessageSystemAttributeName{
			types.MessageSystemAttributeNameApproximateReceiveCount,
			types.MessageSystemAttributeNameSentTimestamp,
		},
	})
	if err != nil {
		return nil, fmt.Errorf("receive from dead-letter queue: %w", err)
	}

	messages := make([]Message, 0, len(out.Messages))
	for _, raw := range out.Messages {
		m := Message{
			ReceiptHandle: aws.ToString(raw.ReceiptHandle),
			Body:          aws.ToString(raw.Body),
		}
		m.Receives, _ = strconv.Atoi(
			raw.Attributes[string(types.MessageSystemAttributeNameApproximateReceiveCount)])

		if sent, ok := raw.Attributes[string(types.MessageSystemAttributeNameSentTimestamp)]; ok {
			if ms, err := strconv.ParseInt(sent, 10, 64); err == nil {
				m.FirstSeen = time.UnixMilli(ms).UTC()
			}
		}
		if attr, ok := raw.MessageAttributes[redriveCountAttribute]; ok {
			m.Redrives, _ = strconv.Atoi(aws.ToString(attr.StringValue))
		}
		messages = append(messages, m)
	}
	return messages, nil
}

// release hands messages straight back to the queue.
//
// ChangeMessageVisibility writes its timeout unconditionally, so unlike
// ReceiveMessage it can genuinely say zero. This is the only way to look at a
// dead-letter queue without hiding what you looked at.
func (r *Redriver) release(ctx context.Context, messages []Message) {
	for _, m := range messages {
		// Best effort: failing to release only means the message reappears
		// after the queue's visibility timeout instead of immediately.
		_, _ = r.Client.ChangeMessageVisibility(ctx, &sqs.ChangeMessageVisibilityInput{
			QueueUrl:          aws.String(r.Source),
			ReceiptHandle:     aws.String(m.ReceiptHandle),
			VisibilityTimeout: 0,
		})
	}
}

// Peek reads without consuming, and without hiding what it read.
func (r *Redriver) Peek(ctx context.Context, limit int) ([]Message, error) {
	var seen []Message
	for len(seen) < limit {
		batch, err := r.receive(ctx, int32(limit-len(seen)), 0)
		if err != nil {
			r.release(ctx, seen)
			return nil, err
		}
		if len(batch) == 0 {
			break
		}
		seen = append(seen, batch...)
	}
	r.release(ctx, seen)
	return seen, nil
}

type Stats struct {
	Replayed int
	Skipped  int
	Failed   int
}

// Replay moves messages back to the main queue.
//
// Send first, then delete. A crash between the two redelivers the message,
// which the consumer is built for; deleting first would lose it, which nothing
// can recover from. The order is the whole safety property.
//
// maxRedrives refuses a message that has already been put back that many times.
// Replaying into an unfixed cause is a loop, and a loop that looks like work is
// worse than a queue that is visibly stuck.
func (r *Redriver) Replay(ctx context.Context, limit, maxRedrives int, dryRun bool) (Stats, error) {
	var stats Stats

	// A dry run must be a no-op in every observable sense, not just a durable
	// one. Reserving the messages for thirty seconds leaves the queue looking
	// empty to whatever runs next, so the report is accurate and the operator
	// concludes it worked.
	for stats.Replayed+stats.Skipped+stats.Failed < limit {
		batch, err := r.receive(ctx, int32(limit), 30)
		if err != nil {
			return stats, err
		}
		if len(batch) == 0 {
			break
		}

		if dryRun {
			for _, msg := range batch {
				if msg.Redrives >= maxRedrives {
					stats.Skipped++
					continue
				}
				stats.Replayed++
			}
			// Hand them straight back, then stop: nothing was consumed, so
			// another pass would count the same messages again.
			r.release(ctx, batch)
			break
		}

		for _, msg := range batch {
			if msg.Redrives >= maxRedrives {
				// Put it back now rather than leaving it reserved: it was not
				// touched, and the next operator should be able to see it.
				r.release(ctx, []Message{msg})
				stats.Skipped++
				continue
			}

			_, err := r.Client.SendMessage(ctx, &sqs.SendMessageInput{
				QueueUrl:    aws.String(r.Target),
				MessageBody: aws.String(msg.Body),
				MessageAttributes: map[string]types.MessageAttributeValue{
					redriveCountAttribute: {
						DataType:    aws.String("Number"),
						StringValue: aws.String(strconv.Itoa(msg.Redrives + 1)),
					},
				},
			})
			if err != nil {
				stats.Failed++
				continue
			}

			if _, err := r.Client.DeleteMessage(ctx, &sqs.DeleteMessageInput{
				QueueUrl:      aws.String(r.Source),
				ReceiptHandle: aws.String(msg.ReceiptHandle),
			}); err != nil {
				// Already sent. Leaving it here means one duplicate, which the
				// consumer handles; the alternative is losing it.
				stats.Failed++
				continue
			}
			stats.Replayed++
		}
	}

	return stats, nil
}

// Purge empties the dead-letter queue.
//
// Unrecoverable, and the only operation here that destroys evidence, so the
// command layer makes the caller say so explicitly.
func (r *Redriver) Purge(ctx context.Context) error {
	_, err := r.Client.PurgeQueue(ctx, &sqs.PurgeQueueInput{QueueUrl: aws.String(r.Source)})
	if err != nil {
		return fmt.Errorf("purge dead-letter queue: %w", err)
	}
	return nil
}
