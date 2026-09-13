// Package awsx builds the AWS clients this platform uses.
//
// One place decides where AWS is. Everything else takes a client and cannot
// tell whether it is talking to LocalStack or to Amazon - which is the point:
// the SDK calls, the request shapes and the error types are identical, so the
// only thing a real deployment changes is an endpoint and a set of
// credentials.
package awsx

import (
	"context"
	"fmt"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/secretsmanager"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
)

// Config is what Load needs to decide where AWS is.
type Config struct {
	Region string
	// Endpoint points at LocalStack. Empty means the real AWS, resolved by the
	// SDK's normal endpoint rules. Load applies it to every client built from
	// the returned aws.Config, so no caller needs to carry it a second time.
	Endpoint string
	// Static credentials for LocalStack. Left empty in a real deployment so the
	// SDK uses the environment, the shared config, or the instance role -
	// which is what you want, because a long-lived key in configuration is the
	// credential that ends up in a git history.
	AccessKeyID     string
	SecretAccessKey string
}

// Load resolves region, credentials and endpoint once; the service
// constructors below take the result.
func Load(ctx context.Context, cfg Config) (aws.Config, error) {
	options := []func(*config.LoadOptions) error{
		config.WithRegion(cfg.Region),
	}
	if cfg.Endpoint != "" {
		options = append(options, config.WithBaseEndpoint(cfg.Endpoint))
	}

	if cfg.AccessKeyID != "" {
		options = append(options, config.WithCredentialsProvider(
			credentials.NewStaticCredentialsProvider(cfg.AccessKeyID, cfg.SecretAccessKey, ""),
		))
	}

	loaded, err := config.LoadDefaultConfig(ctx, options...)
	if err != nil {
		return aws.Config{}, fmt.Errorf("load aws config: %w", err)
	}
	return loaded, nil
}

// SQS returns a client.
func SQS(cfg aws.Config) *sqs.Client {
	return sqs.NewFromConfig(cfg)
}

// S3 returns a client.
//
// UsePathStyle matters for LocalStack: the modern S3 addressing scheme puts the
// bucket in the hostname (bucket.s3.amazonaws.com), which needs DNS that does
// not exist locally. Path style keeps the bucket in the URL path instead. A
// custom endpoint is the signal that we are local.
func S3(cfg aws.Config) *s3.Client {
	return s3.NewFromConfig(cfg, func(o *s3.Options) {
		o.UsePathStyle = cfg.BaseEndpoint != nil
	})
}

// SecretsManager returns a client.
func SecretsManager(cfg aws.Config) *secretsmanager.Client {
	return secretsmanager.NewFromConfig(cfg)
}
