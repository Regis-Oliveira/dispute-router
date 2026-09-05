# The evidence bucket: delivery confirmations, signed receipts, screenshots.
# Customer documents, in other words, which is why most of this file is about
# who cannot read it.

resource "aws_s3_bucket" "evidence" {
  bucket = "${local.name}-evidence"
}

# Four separate settings, and all four matter. This is the resource that turns
# "we had a breach" into "we had a bucket".
resource "aws_s3_bucket_public_access_block" "evidence" {
  bucket = aws_s3_bucket.evidence.id

  block_public_acls       = true
  block_public_policy     = true
  ignore_public_acls      = true
  restrict_public_buckets = true
}

resource "aws_s3_bucket_server_side_encryption_configuration" "evidence" {
  bucket = aws_s3_bucket.evidence.id

  rule {
    apply_server_side_encryption_by_default {
      sse_algorithm = "AES256"
    }
  }
}

resource "aws_s3_bucket_versioning" "evidence" {
  bucket = aws_s3_bucket.evidence.id

  versioning_configuration {
    # Evidence is the thing you produce when somebody disputes what happened.
    # A deleted or overwritten object is exactly what you cannot afford, and
    # versioning is also the cheapest defence against a ransomware event.
    status = "Enabled"
  }
}

# The browser posts a form straight here with a signed policy, so S3 itself has
# to allow the dashboard's origin. The API is not in that request path at all
# and its CORS rules do not apply.
resource "aws_s3_bucket_cors_configuration" "evidence" {
  bucket = aws_s3_bucket.evidence.id

  cors_rule {
    allowed_origins = [var.dashboard_origin]
    allowed_methods = ["GET", "POST", "HEAD"]
    allowed_headers = ["*"]
    expose_headers  = ["ETag"]
    max_age_seconds = 600
  }
}

resource "aws_s3_bucket_lifecycle_configuration" "evidence" {
  bucket = aws_s3_bucket.evidence.id

  rule {
    id     = "expire-old-versions"
    status = "Enabled"

    filter {}

    noncurrent_version_expiration {
      noncurrent_days = 90
    }

    abort_incomplete_multipart_upload {
      # An abandoned multipart upload is invisible in the console and billed
      # anyway. This is the cheapest line in any S3 configuration.
      days_after_initiation = 7
    }
  }
}

# Webhook signing keys.
#
# The value is managed outside Terraform on purpose: putting a credential in a
# .tf file puts it in git, and putting it in state puts it in the state bucket
# in plaintext. Terraform creates the container; something else fills it.
resource "aws_secretsmanager_secret" "webhook_secrets" {
  name        = "${local.name}/webhook-secrets"
  description = "Per-merchant webhook signing keys, newest first. Rotated by prepending."

  # Days before a deleted secret is really gone. Deleting the signing keys is
  # an outage for every merchant at once, so it should be undoable.
  recovery_window_in_days = 14
}

# Terraform manages the secret, not its contents. Without this, an out-of-band
# rotation shows up as drift on every plan and somebody eventually "fixes" it.
resource "aws_secretsmanager_secret_version" "webhook_secrets_placeholder" {
  secret_id     = aws_secretsmanager_secret.webhook_secrets.id
  secret_string = jsonencode({ managed_outside_terraform = true })

  lifecycle {
    ignore_changes = [secret_string]
  }
}
