// Package secrets resolves the webhook signing keys.
//
// A signing key is a credential. Kept in an application table it is readable by
// anything holding a database connection: every service, every migration, every
// analyst with production read access, and every backup of that table. Moving
// it to a secret store narrows that to whoever holds one IAM permission, and
// makes reading it an auditable event rather than an ordinary SELECT.
package secrets

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/secretsmanager"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Resolver answers "which secrets may this merchant have signed with".
//
// Plural, because rotation is not an instant: see signing.VerifyAny.
type Resolver interface {
	SecretsFor(ctx context.Context, merchantExternalID string) ([]string, error)
}

// FromDatabase reads merchants.webhook_secret.
//
// The original arrangement, kept because it is what makes the system runnable
// without AWS at all - and because a secret store you cannot fall back from is
// a single point of failure wearing a security badge.
type FromDatabase struct {
	Pool *pgxpool.Pool
}

func (d FromDatabase) SecretsFor(ctx context.Context, merchantExternalID string) ([]string, error) {
	var secret string
	err := d.Pool.QueryRow(ctx,
		`SELECT webhook_secret FROM merchants WHERE external_id = $1`, merchantExternalID,
	).Scan(&secret)
	if err != nil {
		return nil, fmt.Errorf("load secret for %s: %w", merchantExternalID, err)
	}
	return []string{secret}, nil
}

// FromManager reads one Secrets Manager entry holding every merchant's keys.
//
// One secret rather than one per merchant: Secrets Manager bills per secret per
// month, and the read is cached, so a thousand merchants would be a thousand
// line items to solve a problem that does not exist. The trade is that rotating
// one merchant's key rewrites the whole document.
//
// The value is a map of merchant id to keys, newest first:
//
//	{"mrc_northwind": ["whsec_new", "whsec_previous"]}
type FromManager struct {
	Client   *secretsmanager.Client
	SecretID string
	// TTL bounds how long a rotated key takes to take effect. It is the real
	// rotation latency, so it wants to be minutes, not hours.
	TTL time.Duration

	mu        sync.RWMutex
	cache     map[string][]string
	fetchedAt time.Time

	// refresh serialises concurrent misses. Without it, a cold cache under load
	// sends one API call per in-flight request to a service with a rate limit.
	refresh sync.Mutex
}

func NewFromManager(client *secretsmanager.Client, secretID string, ttl time.Duration) *FromManager {
	return &FromManager{Client: client, SecretID: secretID, TTL: ttl}
}

func (m *FromManager) SecretsFor(ctx context.Context, merchantExternalID string) ([]string, error) {
	if cached, ok := m.lookup(merchantExternalID); ok {
		return cached, nil
	}

	if err := m.load(ctx); err != nil {
		return nil, err
	}

	secrets, ok := m.lookup(merchantExternalID)
	if !ok {
		// Deliberately not an error that says "no such merchant": the caller
		// turns this into the same 401 a bad signature gets, so the endpoint
		// cannot be used to enumerate which merchants exist.
		return nil, nil
	}
	return secrets, nil
}

func (m *FromManager) lookup(merchantExternalID string) ([]string, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	if m.cache == nil || time.Since(m.fetchedAt) > m.TTL {
		return nil, false
	}
	secrets, ok := m.cache[merchantExternalID]
	return secrets, ok
}

func (m *FromManager) load(ctx context.Context) error {
	m.refresh.Lock()
	defer m.refresh.Unlock()

	// Somebody else may have refreshed while this call waited for the lock.
	m.mu.RLock()
	fresh := m.cache != nil && time.Since(m.fetchedAt) <= m.TTL
	m.mu.RUnlock()
	if fresh {
		return nil
	}

	out, err := m.Client.GetSecretValue(ctx, &secretsmanager.GetSecretValueInput{
		SecretId: aws.String(m.SecretID),
	})
	if err != nil {
		return fmt.Errorf("read %s: %w", m.SecretID, err)
	}

	raw := aws.ToString(out.SecretString)
	if raw == "" {
		return fmt.Errorf("%s has no string value", m.SecretID)
	}

	// Each merchant maps to one key or a list of them; both shapes are accepted
	// so a document does not have to be rewritten wholesale to start rotating a
	// single merchant.
	var loose map[string]json.RawMessage
	if err := json.Unmarshal([]byte(raw), &loose); err != nil {
		return fmt.Errorf("parse %s: %w", m.SecretID, err)
	}

	parsed := make(map[string][]string, len(loose))
	for merchant, value := range loose {
		var list []string
		if err := json.Unmarshal(value, &list); err == nil {
			parsed[merchant] = list
			continue
		}
		var single string
		if err := json.Unmarshal(value, &single); err == nil {
			parsed[merchant] = []string{single}
			continue
		}
		// A key that cannot be read is skipped rather than failing the whole
		// document: one malformed entry must not lock out every merchant.
	}

	m.mu.Lock()
	m.cache = parsed
	m.fetchedAt = time.Now()
	m.mu.Unlock()
	return nil
}

// Count is what the startup log reports. The keys themselves are never logged,
// which is the whole point of moving them.
func (m *FromManager) Count() int {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return len(m.cache)
}
