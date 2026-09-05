# The queue, in Terraform rather than the bash script it replaces.
#
# infra/localstack-init.sh does the same job imperatively, and comparing them is
# the clearest argument for this tool. The script says how: create this, ignore
# the error if it exists, then fetch the ARN and interpolate it into a JSON
# string. It cannot tell you what it is about to change, cannot notice that
# somebody widened the visibility timeout in the console, and cannot remove what
# it created.
#
# This says what. The ARN below is a reference, not a variable somebody has to
# remember to fetch, and that reference is also what tells Terraform the
# dead-letter queue must exist first. The dependency graph is derived from the
# code rather than from the order the lines are written in.

resource "aws_sqs_queue" "dead_letter" {
  name = "${local.name}-events-dlq"

  # Longer than the main queue's. A message arrives here after already failing
  # for a while, and the point is that a person gets to look at it - if it
  # expires before anyone does, the dead-letter queue was only ever a slower
  # way of dropping it.
  message_retention_seconds = 1209600 # 14 days, the SQS maximum
}

resource "aws_sqs_queue" "events" {
  name = "${local.name}-events"

  # How long a received message is hidden from other consumers. This is the
  # real lock in an SQS system: if the consumer dies, the message reappears and
  # somebody else takes it. It has to comfortably exceed one decision.
  visibility_timeout_seconds = 60

  message_retention_seconds = 345600 # 4 days

  # Long polling. Zero would busy-poll an empty queue and bill for every empty
  # response; twenty seconds is the maximum and means one request per twenty
  # seconds when idle, and an immediate return when not.
  receive_wait_time_seconds = 20

  redrive_policy = jsonencode({
    deadLetterTargetArn = aws_sqs_queue.dead_letter.arn
    # Five failures is not bad luck, it is a bug. Without this a poison message
    # is redelivered forever and the queue never drains.
    maxReceiveCount = 5
  })
}

# Lets the redrive tooling move messages back. Without it, replay is a
# permissions error at exactly the moment somebody needs it to work.
resource "aws_sqs_queue_redrive_allow_policy" "dead_letter" {
  queue_url = aws_sqs_queue.dead_letter.id

  redrive_allow_policy = jsonencode({
    redrivePermission = "byQueue"
    sourceQueueArns   = [aws_sqs_queue.events.arn]
  })
}
