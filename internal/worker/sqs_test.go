//go:build integration

package worker

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"math/rand"
	"os"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	"github.com/aws/aws-sdk-go-v2/service/sqs/types"

	"github.com/regisoliveira/dispute-router/internal/awsx"
	"github.com/regisoliveira/dispute-router/internal/outbox"
)

// Runs against LocalStack, which speaks the real SQS API - so these exercise
// visibility timeouts, receipt handles and the redrive policy for real, not a
// fake that agrees with whatever the code does.
//
// Tagged with the rest of the file because every test here takes a deadline
// index from testRedis, and testRedis flushes database 15. The queues
// themselves are throwaways this file creates and deletes, which on its own
// would not need a tag.
func testSQS(t *testing.T) *sqs.Client {
	t.Helper()

	endpoint := os.Getenv("AWS_ENDPOINT_URL")
	if endpoint == "" {
		t.Skip("AWS_ENDPOINT_URL not set; skipping SQS tests")
	}

	cfg, err := awsx.Load(t.Context(), awsx.Config{
		Region:          "us-east-1",
		Endpoint:        endpoint,
		AccessKeyID:     "test",
		SecretAccessKey: "test",
	})
	if err != nil {
		t.Fatalf("aws config: %v", err)
	}

	client := awsx.SQS(cfg)
	if _, err := client.ListQueues(t.Context(), &sqs.ListQueuesInput{}); err != nil {
		t.Skipf("localstack unreachable: %v", err)
	}
	return client
}

// newTestQueue builds a throwaway queue and its dead-letter queue, with a short
// visibility timeout so redelivery is observable inside a test.
func newTestQueue(t *testing.T, client *sqs.Client, maxReceive int) (queueURL, dlqURL string) {
	t.Helper()
	ctx := t.Context()
	suffix := fmt.Sprintf("%d-%d", time.Now().UnixNano(), rand.Intn(1000))

	dlq, err := client.CreateQueue(ctx, &sqs.CreateQueueInput{
		QueueName: aws.String("test-dlq-" + suffix),
	})
	if err != nil {
		t.Fatalf("create dlq: %v", err)
	}

	attrs, err := client.GetQueueAttributes(ctx, &sqs.GetQueueAttributesInput{
		QueueUrl:       dlq.QueueUrl,
		AttributeNames: []types.QueueAttributeName{types.QueueAttributeNameQueueArn},
	})
	if err != nil {
		t.Fatalf("dlq arn: %v", err)
	}

	redrive, err := json.Marshal(map[string]string{
		"deadLetterTargetArn": attrs.Attributes[string(types.QueueAttributeNameQueueArn)],
		"maxReceiveCount":     fmt.Sprint(maxReceive),
	})
	if err != nil {
		t.Fatalf("marshal redrive: %v", err)
	}

	main, err := client.CreateQueue(ctx, &sqs.CreateQueueInput{
		QueueName: aws.String("test-main-" + suffix),
		Attributes: map[string]string{
			string(types.QueueAttributeNameVisibilityTimeout): "1",
			string(types.QueueAttributeNameRedrivePolicy):     string(redrive),
		},
	})
	if err != nil {
		t.Fatalf("create queue: %v", err)
	}

	t.Cleanup(func() {
		_, _ = client.DeleteQueue(context.Background(), &sqs.DeleteQueueInput{QueueUrl: main.QueueUrl})
		_, _ = client.DeleteQueue(context.Background(), &sqs.DeleteQueueInput{QueueUrl: dlq.QueueUrl})
	})

	return *main.QueueUrl, *dlq.QueueUrl
}

func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func depth(t *testing.T, client *sqs.Client, url string, attr types.QueueAttributeName) string {
	t.Helper()
	out, err := client.GetQueueAttributes(t.Context(), &sqs.GetQueueAttributesInput{
		QueueUrl:       aws.String(url),
		AttributeNames: []types.QueueAttributeName{attr},
	})
	if err != nil {
		t.Fatalf("queue attributes: %v", err)
	}
	return out.Attributes[string(attr)]
}

// A dispute published by the relay comes back out of the queue and is scheduled
// in the deadline index - the whole point of putting SQS in the path.
func TestPublishedDisputeIsScheduledFromTheQueue(t *testing.T) {
	ctx := t.Context()
	client := testSQS(t)
	queueURL, _ := newTestQueue(t, client, 5)

	rdb := testRedis(t)
	deadlines := NewDeadlines(rdb)

	deadline := time.Now().Add(90 * time.Minute).UTC().Truncate(time.Second)
	payload, err := json.Marshal(map[string]any{
		"dispute_id":  4242,
		"deadline_at": deadline,
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	publisher := outbox.SQSPublisher{Client: client, QueueURL: queueURL}
	if err := publisher.Publish(ctx, outbox.Message{
		ID:            1,
		AggregateType: "dispute",
		AggregateID:   4242,
		EventType:     "dispute.received",
		Payload:       payload,
	}); err != nil {
		t.Fatalf("Publish: %v", err)
	}

	consumer := NewConsumer(ConsumerOptions{
		Client: client, QueueURL: queueURL, Deadlines: deadlines,
		Logger: quietLogger(), MaxMessages: 10, WaitTime: 2,
	})

	runCtx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	go func() { _ = consumer.Run(runCtx) }()

	// The dispute should appear in the index, scored at its deadline.
	deadlineFound := false
	for range 40 {
		ids, err := deadlines.Claim(ctx, deadline.Add(time.Minute), 10)
		if err != nil {
			t.Fatalf("Claim: %v", err)
		}
		if len(ids) == 1 && ids[0] == 4242 {
			deadlineFound = true
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	if !deadlineFound {
		t.Fatal("the dispute never reached the deadline index")
	}

	// Handled means deleted. A message that stays on the queue after being
	// processed is redelivered forever.
	for range 30 {
		if depth(t, client, queueURL, types.QueueAttributeNameApproximateNumberOfMessages) == "0" {
			return
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Error("the message was not deleted after being handled")
}

// The gap Phase 3 left open. A message that cannot be handled must not be
// deleted to keep the logs quiet, and must not be retried forever either - the
// redrive policy takes it off the main queue after maxReceiveCount attempts.
func TestAPoisonMessageEndsUpInTheDeadLetterQueue(t *testing.T) {
	ctx := t.Context()
	client := testSQS(t)
	queueURL, dlqURL := newTestQueue(t, client, 2)

	if _, err := client.SendMessage(ctx, &sqs.SendMessageInput{
		QueueUrl: aws.String(queueURL),
		// Well-formed envelope, unusable payload: the consumer can read the
		// event type but not act on it.
		MessageBody: aws.String(`{"outbox_id":1,"event_type":"dispute.received","payload":{"nonsense":true}}`),
	}); err != nil {
		t.Fatalf("SendMessage: %v", err)
	}

	consumer := NewConsumer(ConsumerOptions{
		Client: client, QueueURL: queueURL, Deadlines: NewDeadlines(testRedis(t)),
		Logger: quietLogger(), MaxMessages: 10, WaitTime: 1,
	})

	runCtx, cancel := context.WithTimeout(ctx, 25*time.Second)
	defer cancel()
	go func() { _ = consumer.Run(runCtx) }()

	for range 60 {
		if depth(t, client, dlqURL, types.QueueAttributeNameApproximateNumberOfMessages) == "1" {
			return
		}
		time.Sleep(400 * time.Millisecond)
	}
	t.Error("the poison message never reached the dead-letter queue")
}

// Events the consumer has no work for still have to be acknowledged. A queue
// nobody drains is a queue that fills up.
func TestUnrelatedEventsAreAcknowledged(t *testing.T) {
	ctx := t.Context()
	client := testSQS(t)
	queueURL, _ := newTestQueue(t, client, 5)

	if _, err := client.SendMessage(ctx, &sqs.SendMessageInput{
		QueueUrl:    aws.String(queueURL),
		MessageBody: aws.String(`{"outbox_id":9,"event_type":"dispute.refunded","payload":{}}`),
	}); err != nil {
		t.Fatalf("SendMessage: %v", err)
	}

	consumer := NewConsumer(ConsumerOptions{
		Client: client, QueueURL: queueURL, Deadlines: NewDeadlines(testRedis(t)),
		Logger: quietLogger(), MaxMessages: 10, WaitTime: 1,
	})

	runCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	go func() { _ = consumer.Run(runCtx) }()

	for range 40 {
		if depth(t, client, queueURL, types.QueueAttributeNameApproximateNumberOfMessages) == "0" {
			return
		}
		time.Sleep(250 * time.Millisecond)
	}
	t.Error("an unhandled event type was left on the queue")
}
