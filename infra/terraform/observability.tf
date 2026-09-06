# Logs and the alarms worth waking someone for.
#
# The test for an alarm is whether a person can do something about it at three
# in the morning. High CPU usually fails that test; a queue that is not draining
# does not.

resource "aws_cloudwatch_log_group" "service" {
  for_each = local.services

  name = "/ecs/${local.name}/${each.key}"
  # CloudWatch keeps logs forever by default and bills for the storage. This is
  # a line nobody writes until the invoice arrives.
  retention_in_days = var.log_retention_days
}

# Anything in the dead-letter queue is a message the system could not handle
# five times. There is no healthy number above zero.
resource "aws_cloudwatch_metric_alarm" "dead_letters" {
  alarm_name          = "${local.name}-dead-letters"
  comparison_operator = "GreaterThanThreshold"
  threshold           = 0
  evaluation_periods  = 1
  period              = 300
  namespace           = "AWS/SQS"
  metric_name         = "ApproximateNumberOfMessagesVisible"
  statistic           = "Maximum"

  dimensions = {
    QueueName = aws_sqs_queue.dead_letter.name
  }

  alarm_description  = "Messages the platform could not process. Inspect with `dlq peek`, fix the cause, then `dlq replay`."
  alarm_actions      = var.alarm_topic_arn == "" ? [] : [var.alarm_topic_arn]
  treat_missing_data = "notBreaching"
}

# The one that actually matters for this product.
#
# Queue depth tells you how much is waiting; age tells you how long the oldest
# thing has waited, which is the number that maps onto a missed deadline. A
# backlog of ten thousand that clears in a minute is fine. One message stuck for
# an hour is a dispute nobody is going to answer in time.
resource "aws_cloudwatch_metric_alarm" "queue_age" {
  alarm_name          = "${local.name}-queue-falling-behind"
  comparison_operator = "GreaterThanThreshold"
  threshold           = 900 # 15 minutes
  evaluation_periods  = 2
  period              = 300
  namespace           = "AWS/SQS"
  metric_name         = "ApproximateAgeOfOldestMessage"
  statistic           = "Maximum"

  dimensions = {
    QueueName = aws_sqs_queue.events.name
  }

  alarm_description  = "The oldest queued dispute has waited 15 minutes. Alerts expire in hours, so this is the early warning for a missed window."
  alarm_actions      = var.alarm_topic_arn == "" ? [] : [var.alarm_topic_arn]
  treat_missing_data = "notBreaching"
}

# A service that cannot keep a task running. Distinct from "the service is
# slow": this is a crash loop, and the deployment circuit breaker may already
# have rolled back by the time anyone looks.
resource "aws_cloudwatch_metric_alarm" "task_count" {
  for_each = local.services

  alarm_name          = "${local.name}-${each.key}-tasks-below-desired"
  comparison_operator = "LessThanThreshold"
  threshold           = each.value.desired_count
  evaluation_periods  = 3
  period              = 60
  namespace           = "ECS/ContainerInsights"
  metric_name         = "RunningTaskCount"
  statistic           = "Average"

  dimensions = {
    ClusterName = aws_ecs_cluster.main.name
    ServiceName = aws_ecs_service.main[each.key].name
  }

  alarm_description  = "${each.key} is running fewer tasks than it should."
  alarm_actions      = var.alarm_topic_arn == "" ? [] : [var.alarm_topic_arn]
  treat_missing_data = "breaching"
}
