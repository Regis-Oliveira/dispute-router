package dlq

import (
	"fmt"
	"math/rand"
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	"github.com/aws/aws-sdk-go-v2/service/sqs/types"

	"github.com/regisoliveira/dispute-router/internal/awsx"
)

// Runs against LocalStack, which speaks the real SQS API. The bookkeeping
// under test is about visibility timeouts and receipt handles, which a fake
// would only reproduce as faithfully as the code it agrees with.
func testRedriver(t *testing.T) *Redriver {
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

	suffix := fmt.Sprintf("%d-%d", time.Now().UnixNano(), rand.Intn(1000))
	newQueue := func(name string) string {
		out, err := client.CreateQueue(t.Context(), &sqs.CreateQueueInput{QueueName: aws.String(name + suffix)})
		if err != nil {
			t.Fatalf("create %s: %v", name, err)
		}
		t.Cleanup(func() {
			_, _ = client.DeleteQueue(t.Context(), &sqs.DeleteQueueInput{QueueUrl: out.QueueUrl})
		})
		return *out.QueueUrl
	}
	return &Redriver{
		Client: client,
		Source: newQueue("test-dlq-"),
		Target: newQueue("test-main-"),
	}
}

func send(t *testing.T, r *Redriver, body string, redrives int) {
	t.Helper()
	in := &sqs.SendMessageInput{QueueUrl: aws.String(r.Source), MessageBody: aws.String(body)}
	if redrives > 0 {
		in.MessageAttributes = map[string]types.MessageAttributeValue{
			redriveCountAttribute: {
				DataType:    aws.String("Number"),
				StringValue: aws.String(strconv.Itoa(redrives)),
			},
		}
	}
	if _, err := r.Client.SendMessage(t.Context(), in); err != nil {
		t.Fatalf("send %q: %v", body, err)
	}
}

// visible drains what a queue will hand out right now, and hands it back.
func visible(t *testing.T, r *Redriver, queueURL string) map[string]int {
	t.Helper()
	bodies := map[string]int{}
	var handles []string
	for {
		out, err := r.Client.ReceiveMessage(t.Context(), &sqs.ReceiveMessageInput{
			QueueUrl:              aws.String(queueURL),
			MaxNumberOfMessages:   10,
			WaitTimeSeconds:       1,
			MessageAttributeNames: []string{"All"},
		})
		if err != nil {
			t.Fatalf("receive: %v", err)
		}
		if len(out.Messages) == 0 {
			break
		}
		for _, m := range out.Messages {
			bodies[aws.ToString(m.Body)], _ = strconv.Atoi(aws.ToString(m.MessageAttributes[redriveCountAttribute].StringValue))
			handles = append(handles, aws.ToString(m.ReceiptHandle))
		}
	}
	for _, h := range handles {
		_, _ = r.Client.ChangeMessageVisibility(t.Context(), &sqs.ChangeMessageVisibilityInput{
			QueueUrl: aws.String(queueURL), ReceiptHandle: aws.String(h), VisibilityTimeout: 0,
		})
	}
	return bodies
}

// One over-redriven message is counted Skipped once, not once per receive
// until limit, and is visible again the moment Replay returns.
func TestReplaySkipsOnce(t *testing.T) {
	r := testRedriver(t)
	send(t, r, "fresh", 0)
	send(t, r, "once", 1)
	send(t, r, "exhausted", 3)

	stats, err := r.Replay(t.Context(), 10, 3, false)
	if err != nil {
		t.Fatalf("replay: %v", err)
	}
	if stats.Errors != nil {
		t.Fatalf("replay errors: %v", stats.Errors)
	}
	if stats.Replayed != 2 || stats.Skipped != 1 || stats.Failed != 0 {
		t.Fatalf("stats = %+v, want replayed 2, skipped 1, failed 0", stats)
	}

	// The skipped message was never touched, so the next operator sees it now
	// rather than after the visibility timeout.
	if got := visible(t, r, r.Source); len(got) != 1 || got["exhausted"] != 3 {
		t.Errorf("dead-letter queue shows %v, want only exhausted at 3 redrives", got)
	}
	got := visible(t, r, r.Target)
	if len(got) != 2 || got["fresh"] != 1 || got["once"] != 2 {
		t.Errorf("main queue shows %v, want fresh at 1 and once at 2 redrives", got)
	}
}

func TestReplayDryRunChangesNothing(t *testing.T) {
	r := testRedriver(t)
	send(t, r, "fresh", 0)
	send(t, r, "exhausted", 3)

	stats, err := r.Replay(t.Context(), 10, 3, true)
	if err != nil {
		t.Fatalf("replay: %v", err)
	}
	if stats.Replayed != 1 || stats.Skipped != 1 || stats.Failed != 0 || stats.Errors != nil {
		t.Fatalf("stats = %+v, want would-replay 1, skipped 1", stats)
	}
	if got := visible(t, r, r.Source); len(got) != 2 {
		t.Errorf("dead-letter queue shows %v, want both messages still visible", got)
	}
	if got := visible(t, r, r.Target); len(got) != 0 {
		t.Errorf("main queue shows %v, want nothing", got)
	}
}
