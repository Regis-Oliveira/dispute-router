#!/usr/bin/env bash
#
# Creates the AWS resources this platform needs, in LocalStack.
#
# Run through `make aws-init`. Idempotent: every call either creates the
# resource or accepts that it already exists, so re-running after a change is
# safe and is the normal way to apply one.
#
# `awslocal` is bundled in the LocalStack image and is just the AWS CLI with
# --endpoint-url pre-set, so every command below is the real AWS CLI command.
set -euo pipefail

AWS="docker exec dr-localstack awslocal"

QUEUE=disputes-events
DLQ=disputes-events-dlq
BUCKET=dispute-evidence

echo "==> dead-letter queue"
$AWS sqs create-queue --queue-name "$DLQ" >/dev/null
DLQ_ARN=$($AWS sqs get-queue-attributes \
  --queue-url "http://sqs.us-east-1.localhost.localstack.cloud:4566/000000000000/$DLQ" \
  --attribute-names QueueArn --query 'Attributes.QueueArn' --output text)
echo "    $DLQ_ARN"

echo "==> main queue with a redrive policy"
# Built with python rather than a heredoc because RedrivePolicy is JSON *inside*
# a JSON string, and getting that through two layers of shell quoting silently
# produced a policy with no deadLetterTargetArn in it - a redrive policy that
# looks configured and drops nothing.
#
# maxReceiveCount 5: a message that has failed five times will not succeed on
# the sixth. Without this a poison message is redelivered forever and the queue
# never drains - exactly the gap the worker still had at the end of Phase 3.
#
# VisibilityTimeout 60: how long a received message is hidden from other
# consumers. This is the real lock in an SQS system - if the consumer dies the
# message reappears and somebody else takes it - so it has to comfortably
# exceed how long one decision takes.
ATTRS=$(python3 -c '
import json, sys
redrive = json.dumps({"deadLetterTargetArn": sys.argv[1], "maxReceiveCount": "5"})
print(json.dumps({
    "VisibilityTimeout": "60",
    "MessageRetentionPeriod": "345600",
    "ReceiveMessageWaitTimeSeconds": "20",
    "RedrivePolicy": redrive,
}))
' "$DLQ_ARN")

$AWS sqs create-queue --queue-name "$QUEUE" --attributes "$ATTRS" >/dev/null

QUEUE_URL=$($AWS sqs get-queue-url --queue-name "$QUEUE" --query 'QueueUrl' --output text)
echo "    $QUEUE_URL"

echo "==> evidence bucket"
$AWS s3api create-bucket --bucket "$BUCKET" 2>/dev/null || echo "    (already exists)"

# The browser POSTs a multipart form directly to S3 with a signed policy, so S3
# itself has to allow the dashboard's origin - the Go service is not in that
# request path at all, and its CORS rules do not apply to it.
$AWS s3api put-bucket-cors --bucket "$BUCKET" --cors-configuration "$(cat <<'JSON'
{
  "CORSRules": [
    {
      "AllowedOrigins": ["http://localhost:4200"],
      "AllowedMethods": ["GET", "POST", "HEAD"],
      "AllowedHeaders": ["*"],
      "ExposeHeaders": ["ETag"],
      "MaxAgeSeconds": 600
    }
  ]
}
JSON
)" >/dev/null
echo "    cors set for http://localhost:4200"

echo "==> webhook signing secrets"
# The keys currently live in merchants.webhook_secret, where anything with a
# database connection can read them. This publishes them where one IAM
# permission can, and where reading them is an auditable event.
#
# Each merchant maps to a LIST of keys, newest first, so a key can be rotated
# by prepending the new one and dropping the old one a while later - rather
# than a cutover that rejects every delivery already in flight.
SECRETS=$(docker exec dr-postgres psql -U dispute -d dispute_router -t -A -c \
  "SELECT jsonb_pretty(jsonb_object_agg(external_id, jsonb_build_array(webhook_secret))) FROM merchants;" 2>/dev/null)

if [ -z "$SECRETS" ] || [ "$SECRETS" = "" ]; then
  echo "    (no merchants yet - run make seed, then make aws-init again)"
else
  $AWS secretsmanager create-secret --name dispute-router/webhook-secrets \
    --secret-string "$SECRETS" >/dev/null 2>&1 \
    || $AWS secretsmanager put-secret-value --secret-id dispute-router/webhook-secrets \
         --secret-string "$SECRETS" >/dev/null
  echo "    published keys for $(echo "$SECRETS" | grep -c 'whsec_') merchant(s)"
fi

echo
echo "ready:"
echo "  queue     $QUEUE_URL"
echo "  dlq       $DLQ"
echo "  bucket    s3://$BUCKET"
