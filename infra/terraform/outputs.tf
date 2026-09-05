# Outputs are the interface of this stack.
#
# They are what another Terraform project reads with a remote-state data source,
# and what a deploy pipeline reads with `terraform output -json`. Anything a
# human has to find in the console instead is a gap.

output "webhook_endpoint" {
  description = "Where the payment processor sends webhooks."
  value       = "https://${aws_lb.main.dns_name}/webhooks/processor"
}

output "api_endpoint" {
  description = "Where the dashboard calls."
  value       = "https://${aws_lb.main.dns_name}/api"
}

output "queue_url" {
  value = aws_sqs_queue.events.url
}

output "dead_letter_queue_url" {
  description = "Pass as SQS_DLQ_URL to the dlq command."
  value       = aws_sqs_queue.dead_letter.url
}

output "evidence_bucket" {
  value = aws_s3_bucket.evidence.id
}

output "webhook_secret_id" {
  description = "Secrets Manager entry holding the signing keys. The value is managed outside Terraform."
  value       = aws_secretsmanager_secret.webhook_secrets.name
}

output "ecr_repository_urls" {
  description = "Where the build pipeline pushes."
  value       = { for name, repo in aws_ecr_repository.service : name => repo.repository_url }
}

output "task_role_arns" {
  description = "One per service. Useful when granting access to something this stack does not own."
  value       = { for name, role in aws_iam_role.task : name => role.arn }
}
