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

type Config struct {
	Region string
	// Endpoint points at LocalStack. Empty means the real AWS, resolved by the
	// SDK's normal endpoint rules.
	Endpoint string
	// Static credentials for LocalStack. Left empty in a real deployment so the
	// SDK uses the environment, the shared config, or the instance role -
	// which is what you want, because a long-lived key in configuration is the
	// credential that ends up in a git history.
	AccessKeyID     string
	SecretAccessKey string
}

func Load(ctx context.Context, cfg Config) (aws.Config, error) {
	options := []func(*config.LoadOptions) error{
		config.WithRegion(cfg.Region),
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

// SQS returns a client, pointed at the endpoint when one is set.
func SQS(cfg aws.Config, endpoint string) *sqs.Client {
	return sqs.NewFromConfig(cfg, func(o *sqs.Options) {
		if endpoint != "" {
			o.BaseEndpoint = aws.String(endpoint)
		}
	})
}

// S3 returns a client.
//
// UsePathStyle matters for LocalStack: the modern S3 addressing scheme puts the
// bucket in the hostname (bucket.s3.amazonaws.com), which needs DNS that does
// not exist locally. Path style keeps the bucket in the URL path instead.
func S3(cfg aws.Config, endpoint string) *s3.Client {
	return s3.NewFromConfig(cfg, func(o *s3.Options) {
		if endpoint != "" {
			o.BaseEndpoint = aws.String(endpoint)
			o.UsePathStyle = true
		}
	})
}

// Presigner mints URLs the browser can use directly.
func Presigner(client *s3.Client) *s3.PresignClient {
	return s3.NewPresignClient(client)
}

// SecretsManager returns a client.
func SecretsManager(cfg aws.Config, endpoint string) *secretsmanager.Client {
	return secretsmanager.NewFromConfig(cfg, func(o *secretsmanager.Options) {
		if endpoint != "" {
			o.BaseEndpoint = aws.String(endpoint)
		}
	})
}
