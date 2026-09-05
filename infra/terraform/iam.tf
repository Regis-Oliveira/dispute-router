# IAM, and the one distinction worth getting right.
#
# An ECS task has TWO roles, they are used by different things at different
# times, and confusing them is the most common ECS failure there is:
#
#   execution role - assumed by the ECS agent, BEFORE your container starts.
#                    It pulls the image, creates the log stream, and resolves
#                    any `secrets` in the task definition. Your code never uses
#                    it. Symptom of getting it wrong: the task never starts, and
#                    the error is in the ECS event log rather than your logs.
#
#   task role      - assumed by YOUR PROCESS, at runtime. This is what the AWS
#                    SDK picks up when it looks for credentials, and what
#                    authorises every SQS and S3 call the service makes.
#                    Symptom of getting it wrong: the task starts fine and then
#                    returns AccessDenied on its first real request.
#
# The other thing this file is about is scope. Every policy below names the
# exact queue or bucket it may touch. `Resource = "*"` would work, would never
# fail a test, and would mean a compromised worker can read every bucket in the
# account.

# ---------------------------------------------------------------------------
# Trust policy: who may assume these roles
# ---------------------------------------------------------------------------

# Written as a data source rather than a heredoc of JSON. It is validated at
# plan time, it interpolates without quoting games, and a typo becomes an error
# instead of a policy that silently matches nothing.
data "aws_iam_policy_document" "ecs_tasks_assume" {
  statement {
    effect  = "Allow"
    actions = ["sts:AssumeRole"]

    principals {
      type        = "Service"
      identifiers = ["ecs-tasks.amazonaws.com"]
    }

    # Without this, any ECS task in any account that guesses the role ARN could
    # assume it - the confused deputy problem, in the service that has the most
    # opportunity for it.
    condition {
      test     = "ArnLike"
      variable = "aws:SourceArn"
      values   = ["arn:aws:ecs:${var.region}:${data.aws_caller_identity.current.account_id}:*"]
    }

    condition {
      test     = "StringEquals"
      variable = "aws:SourceAccount"
      values   = [data.aws_caller_identity.current.account_id]
    }
  }
}

# ---------------------------------------------------------------------------
# Execution role - one, shared. It does the same job for every service.
# ---------------------------------------------------------------------------

resource "aws_iam_role" "execution" {
  name               = "${local.name}-execution"
  assume_role_policy = data.aws_iam_policy_document.ecs_tasks_assume.json
  description        = "Used by the ECS agent to start tasks. Never by application code."
}

# AWS maintains this one: pull from ECR, write to CloudWatch Logs. Worth using
# rather than reimplementing, because AWS updates it when the requirements change.
resource "aws_iam_role_policy_attachment" "execution_managed" {
  role       = aws_iam_role.execution.name
  policy_arn = "arn:aws:iam::aws:policy/service-role/AmazonECSTaskExecutionRolePolicy"
}

# Resolving `secrets` in a task definition happens before the container starts,
# so this permission belongs to the execution role - not to the task role that
# the code runs as. Getting this backwards produces a task that will not start
# and an error nowhere near the application.
data "aws_iam_policy_document" "execution_secrets" {
  statement {
    sid       = "ReadInjectedSecrets"
    effect    = "Allow"
    actions   = ["secretsmanager:GetSecretValue"]
    resources = [var.database_url_secret_arn]
  }
}

resource "aws_iam_role_policy" "execution_secrets" {
  name   = "read-injected-secrets"
  role   = aws_iam_role.execution.id
  policy = data.aws_iam_policy_document.execution_secrets.json
}

# ---------------------------------------------------------------------------
# Task roles - one each, because the three services need different things
# ---------------------------------------------------------------------------

resource "aws_iam_role" "task" {
  for_each = local.services

  name               = "${local.name}-${each.key}-task"
  assume_role_policy = data.aws_iam_policy_document.ecs_tasks_assume.json
  description        = "Assumed by the ${each.key} process at runtime."
}

# --- ingest: writes to the queue, reads the signing keys --------------------

data "aws_iam_policy_document" "ingest" {
  statement {
    sid    = "PublishOutboxMessages"
    effect = "Allow"
    actions = [
      "sqs:SendMessage",
      "sqs:GetQueueUrl",
    ]
    resources = [aws_sqs_queue.events.arn]
  }

  statement {
    sid       = "ReadWebhookSigningKeys"
    effect    = "Allow"
    actions   = ["secretsmanager:GetSecretValue"]
    resources = [aws_secretsmanager_secret.webhook_secrets.arn]
  }
}

# --- worker: reads the queue, and cannot write to it ------------------------

data "aws_iam_policy_document" "worker" {
  statement {
    sid    = "ConsumeEvents"
    effect = "Allow"
    actions = [
      "sqs:ReceiveMessage",
      # Deleting is the acknowledgement, so it is as necessary as receiving.
      "sqs:DeleteMessage",
      "sqs:ChangeMessageVisibility",
      "sqs:GetQueueAttributes",
      "sqs:GetQueueUrl",
    ]
    resources = [aws_sqs_queue.events.arn]
  }

  # Note what is missing: sqs:SendMessage. The worker consumes and decides; it
  # has no business putting messages on the queue it drains, and a policy that
  # allowed it would hide a whole class of accidental loop.
}

# --- api: reads the bucket, and signs upload policies -----------------------

data "aws_iam_policy_document" "api" {
  statement {
    sid    = "ListEvidence"
    effect = "Allow"
    actions = ["s3:ListBucket"]
    # Listing is a permission on the BUCKET; reading is a permission on the
    # OBJECTS. Two different ARNs, and mixing them up is why "it can list but
    # not download" is such a common afternoon.
    resources = [aws_s3_bucket.evidence.arn]

    condition {
      test     = "StringLike"
      variable = "s3:prefix"
      values   = ["disputes/*"]
    }
  }

  statement {
    sid    = "ReadAndSignEvidence"
    effect = "Allow"
    actions = [
      "s3:GetObject",
      # Needed even though this service never uploads anything itself.
      #
      # Presigning is local: the SDK signs a URL with the caller's credentials
      # and makes no API call, so nothing fails at signing time. S3 checks the
      # permission when the URL is USED - so a missing s3:PutObject here shows
      # up as a browser upload failing, with no error on the server at all.
      "s3:PutObject",
    ]
    resources = ["${aws_s3_bucket.evidence.arn}/disputes/*"]
  }
}

# --- attach ------------------------------------------------------------------

# One place, driven by the map. Adding a service means adding its policy
# document and an entry here, and forgetting the attachment is a plan diff
# rather than a runtime discovery.
resource "aws_iam_role_policy" "task" {
  for_each = {
    ingest = data.aws_iam_policy_document.ingest.json
    worker = data.aws_iam_policy_document.worker.json
    api    = data.aws_iam_policy_document.api.json
  }

  name   = "${local.name}-${each.key}"
  role   = aws_iam_role.task[each.key].id
  policy = each.value
}

# Every task gets this: it is what makes `aws ecs execute-command` work, which
# is how you get a shell in a running container without a bastion host or an
# SSH key anywhere in the estate.
data "aws_iam_policy_document" "exec_command" {
  statement {
    sid    = "SessionManagerChannel"
    effect = "Allow"
    actions = [
      "ssmmessages:CreateControlChannel",
      "ssmmessages:CreateDataChannel",
      "ssmmessages:OpenControlChannel",
      "ssmmessages:OpenDataChannel",
    ]
    # These actions do not take a resource, which is why this is the one "*"
    # in the file - and why it is called out rather than left to be noticed.
    resources = ["*"]
  }
}

resource "aws_iam_role_policy" "exec_command" {
  for_each = local.services

  name   = "ecs-exec"
  role   = aws_iam_role.task[each.key].id
  policy = data.aws_iam_policy_document.exec_command.json
}
