package secrets

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/secretsmanager"

	"github.com/regisoliveira/dispute-router/internal/awsx"
)

func testManager(t *testing.T, document string, ttl time.Duration) *FromManager {
	t.Helper()

	endpoint := os.Getenv("AWS_ENDPOINT_URL")
	if endpoint == "" {
		t.Skip("AWS_ENDPOINT_URL not set; skipping Secrets Manager tests")
	}

	cfg, err := awsx.Load(context.Background(), awsx.Config{
		Region: "us-east-1", Endpoint: endpoint,
		AccessKeyID: "test", SecretAccessKey: "test",
	})
	if err != nil {
		t.Fatalf("aws config: %v", err)
	}

	client := awsx.SecretsManager(cfg)
	name := "test/webhook-secrets-" + time.Now().Format("150405.000000")

	if _, err := client.CreateSecret(context.Background(), &secretsmanager.CreateSecretInput{
		Name:         aws.String(name),
		SecretString: aws.String(document),
	}); err != nil {
		t.Skipf("localstack unreachable: %v", err)
	}

	t.Cleanup(func() {
		_, _ = client.DeleteSecret(context.Background(), &secretsmanager.DeleteSecretInput{
			SecretId:                   aws.String(name),
			ForceDeleteWithoutRecovery: aws.Bool(true),
		})
	})

	return NewFromManager(client, name, ttl)
}

func TestReadsBothShapes(t *testing.T) {
	// A single string and a list both work, so a document does not have to be
	// rewritten wholesale to start rotating one merchant.
	m := testManager(t, `{
		"mrc_single": "whsec_only",
		"mrc_rotating": ["whsec_new", "whsec_old"]
	}`, time.Minute)

	ctx := context.Background()

	single, err := m.SecretsFor(ctx, "mrc_single")
	if err != nil {
		t.Fatalf("SecretsFor: %v", err)
	}
	if len(single) != 1 || single[0] != "whsec_only" {
		t.Errorf("single = %v", single)
	}

	rotating, err := m.SecretsFor(ctx, "mrc_rotating")
	if err != nil {
		t.Fatalf("SecretsFor: %v", err)
	}
	if len(rotating) != 2 || rotating[0] != "whsec_new" {
		t.Errorf("rotating = %v, want the new key first", rotating)
	}
}

// An unknown merchant is not an error. The caller turns it into the same 401 a
// bad signature gets, so the endpoint cannot be used to find out which
// merchants exist.
func TestUnknownMerchantIsEmptyNotAnError(t *testing.T) {
	m := testManager(t, `{"mrc_known": "whsec_a"}`, time.Minute)

	got, err := m.SecretsFor(context.Background(), "mrc_does_not_exist")
	if err != nil {
		t.Fatalf("SecretsFor = %v, want no error", err)
	}
	if len(got) != 0 {
		t.Errorf("SecretsFor = %v, want empty", got)
	}
}

// One malformed entry must not lock out every other merchant.
func TestAMalformedEntryDoesNotBreakTheRest(t *testing.T) {
	m := testManager(t, `{"mrc_good": "whsec_a", "mrc_bad": 12345}`, time.Minute)

	good, err := m.SecretsFor(context.Background(), "mrc_good")
	if err != nil {
		t.Fatalf("SecretsFor: %v", err)
	}
	if len(good) != 1 {
		t.Errorf("the good merchant returned %v", good)
	}

	bad, err := m.SecretsFor(context.Background(), "mrc_bad")
	if err != nil {
		t.Errorf("a malformed entry produced an error: %v", err)
	}
	if len(bad) != 0 {
		t.Errorf("mrc_bad = %v, want empty", bad)
	}
}

// The read is cached, because otherwise every webhook is an API call to a
// rate-limited service.
func TestTheDocumentIsCached(t *testing.T) {
	m := testManager(t, `{"mrc_a": "whsec_a"}`, time.Minute)
	ctx := context.Background()

	if _, err := m.SecretsFor(ctx, "mrc_a"); err != nil {
		t.Fatalf("SecretsFor: %v", err)
	}
	first := m.fetchedAt

	for range 20 {
		if _, err := m.SecretsFor(ctx, "mrc_a"); err != nil {
			t.Fatalf("SecretsFor: %v", err)
		}
	}
	if !m.fetchedAt.Equal(first) {
		t.Error("the document was refetched while still inside its TTL")
	}
	if m.Count() != 1 {
		t.Errorf("Count() = %d, want 1", m.Count())
	}
}

// The TTL is the rotation latency: a key published now takes effect within it.
func TestAnExpiredCacheIsRefetched(t *testing.T) {
	m := testManager(t, `{"mrc_a": "whsec_a"}`, 50*time.Millisecond)
	ctx := context.Background()

	if _, err := m.SecretsFor(ctx, "mrc_a"); err != nil {
		t.Fatalf("SecretsFor: %v", err)
	}
	first := m.fetchedAt

	time.Sleep(120 * time.Millisecond)
	if _, err := m.SecretsFor(ctx, "mrc_a"); err != nil {
		t.Fatalf("SecretsFor: %v", err)
	}
	if m.fetchedAt.Equal(first) {
		t.Error("the document was not refetched after its TTL lapsed")
	}
}
