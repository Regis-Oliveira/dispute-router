package outbox

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	"github.com/aws/aws-sdk-go-v2/service/sqs/types"
)

// SQSPublisher sends outbox messages to a queue.
//
// It slots in behind the same Publisher interface the log stand-in used, which
// is the whole reason the relay never needed to change: the outbox pattern was
// built first, so the queue underneath it is a swappable detail rather than an
// architecture.
type SQSPublisher struct {
	Client   *sqs.Client
	QueueURL string
}

func (p SQSPublisher) Publish(ctx context.Context, msg Message) error {
	body, err := json.Marshal(struct {
		OutboxID      int64           `json:"outbox_id"`
		AggregateType string          `json:"aggregate_type"`
		AggregateID   int64           `json:"aggregate_id"`
		EventType     string          `json:"event_type"`
		Payload       json.RawMessage `json:"payload"`
	}{msg.ID, msg.AggregateType, msg.AggregateID, msg.EventType, msg.Payload})
	if err != nil {
		return fmt.Errorf("marshal message %d: %w", msg.ID, err)
	}

	_, err = p.Client.SendMessage(ctx, &sqs.SendMessageInput{
		QueueUrl:    aws.String(p.QueueURL),
		MessageBody: aws.String(string(body)),
		// Attributes let a consumer filter or route without parsing the body,
		// and they show up in the console, which is worth the few bytes when
		// somebody is staring at a stuck queue at two in the morning.
		MessageAttributes: map[string]types.MessageAttributeValue{
			"event_type": {
				DataType:    aws.String("String"),
				StringValue: aws.String(msg.EventType),
			},
			"outbox_id": {
				DataType:    aws.String("Number"),
				StringValue: aws.String(strconv.FormatInt(msg.ID, 10)),
			},
		},
	})
	if err != nil {
		return fmt.Errorf("send message %d to sqs: %w", msg.ID, err)
	}
	return nil
}
