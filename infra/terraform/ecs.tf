# The cluster, the task definitions and the services.
#
# Three nouns worth separating, because they are easy to blur:
#
#   cluster         - where tasks run. On Fargate it is close to a namespace.
#   task definition - an immutable description of a container: image, cpu,
#                     memory, environment, roles. Registering a new one creates
#                     a new revision; the old ones stay.
#   service         - a running deployment: "keep N copies of revision R alive,
#                     behind this target group". Deploying is a service pointed
#                     at a new revision, which is why a rollback is instant -
#                     the previous revision was never deleted.

resource "aws_ecs_cluster" "main" {
  name = local.name

  setting {
    name  = "containerInsights"
    value = "enabled"
  }
}

resource "aws_ecs_cluster_capacity_providers" "main" {
  cluster_name = aws_ecs_cluster.main.name

  # FARGATE_SPOT is materially cheaper and can be reclaimed with two minutes'
  # notice. Fine for the worker, whose messages simply become visible again;
  # not fine for a webhook receiver that a payment processor is retrying
  # against. So it is available, and the strategy below chooses per service.
  capacity_providers = ["FARGATE", "FARGATE_SPOT"]

  default_capacity_provider_strategy {
    capacity_provider = "FARGATE"
    weight            = 1
  }
}

resource "aws_ecs_task_definition" "service" {
  for_each = local.services

  family                   = "${local.name}-${each.key}"
  requires_compatibilities = ["FARGATE"]

  # Fargate needs its own network interface per task, so this is not a choice.
  network_mode = "awsvpc"

  # Strings, not numbers, and only certain combinations are valid: 512 CPU
  # units accepts 1024-4096 MB and nothing else. Fargate rejects the rest at
  # registration, which is at least a fast failure.
  cpu    = tostring(each.value.cpu)
  memory = tostring(each.value.memory)

  # Roughly 20% cheaper than x86 for the same work, and Go cross-compiles to it
  # with an environment variable. The catch is that the image has to be built
  # for it - a GOARCH=amd64 binary here fails with an exec format error that
  # looks nothing like a platform mismatch.
  runtime_platform {
    cpu_architecture        = "ARM64"
    operating_system_family = "LINUX"
  }

  # The two roles, and the whole reason iam.tf explains the difference.
  execution_role_arn = aws_iam_role.execution.arn
  task_role_arn      = aws_iam_role.task[each.key].arn

  container_definitions = jsonencode([
    {
      name = each.key
      # The tag is a git sha. See the validation on var.image_tag.
      image     = "${aws_ecr_repository.service[each.key].repository_url}:${var.image_tag}"
      essential = true

      # Only the services that serve traffic publish a port. The worker has
      # none, which is not a detail - it is why nothing can reach it.
      portMappings = each.value.public ? [{
        containerPort = each.value.port
        protocol      = "tcp"
      }] : []

      # Plain configuration. Nothing here is a credential.
      environment = [
        { name = "AWS_REGION", value = var.region },
        { name = "SQS_QUEUE_URL", value = aws_sqs_queue.events.url },
        { name = "SQS_DLQ_URL", value = aws_sqs_queue.dead_letter.url },
        { name = "S3_EVIDENCE_BUCKET", value = aws_s3_bucket.evidence.id },
        { name = "REDIS_URL", value = var.redis_url },
        { name = "WEBHOOK_SECRET_SOURCE", value = "secretsmanager" },
        { name = "WEBHOOK_SECRET_ID", value = aws_secretsmanager_secret.webhook_secrets.name },
        { name = "API_CORS_ORIGINS", value = var.dashboard_origin },
        { name = "INGEST_ADDR", value = ":8080" },
        { name = "API_ADDR", value = ":8081" },
        # No AWS_ENDPOINT_URL. Its absence is what switches the SDK from
        # LocalStack to the real thing - the single difference between the two.
      ]

      # Credentials, resolved by the ECS agent before the container starts and
      # never visible in the task definition, the console, or `describe-tasks`.
      #
      # The alternative - putting the connection string in `environment` -
      # writes the database password into an API response that a great many
      # people can read.
      secrets = [
        { name = "DATABASE_URL", valueFrom = var.database_url_secret_arn },
      ]

      logConfiguration = {
        logDriver = "awslogs"
        options = {
          "awslogs-group"         = aws_cloudwatch_log_group.service[each.key].name
          "awslogs-region"        = var.region
          "awslogs-stream-prefix" = "ecs"
        }
      }

      # Defence in depth, and free. A Go binary needs no write access to its own
      # filesystem, so anything that lands one in the container has nowhere to
      # put it.
      readonlyRootFilesystem = true
      user                   = "10001:10001"

      # No container-level healthCheck here, deliberately.
      #
      # ECS's own check runs a command *inside* the container, which means the
      # image needs a shell and something like wget or curl. A Go binary on a
      # scratch base has neither, so the check fails, the task is killed, and
      # the symptom - a container that starts and dies with no application
      # error - looks nothing like its cause.
      #
      # The load balancer's health check in alb.tf does the same job from
      # outside, needs nothing in the image, and is what actually decides
      # whether a task receives traffic. The worker has neither, and does not
      # need one: nothing routes to it, and a crashed task is replaced by ECS
      # and alarmed on by RunningTaskCount in observability.tf.
    }
  ])
}

resource "aws_ecs_service" "main" {
  for_each = local.services

  name    = each.key
  cluster = aws_ecs_cluster.main.id

  # Points at a specific revision. Rolling back is this line pointing at the
  # previous one, which is why an ECS rollback is a minute rather than a build.
  task_definition = aws_ecs_task_definition.service[each.key].arn
  desired_count   = each.value.desired_count

  # Only the worker takes spot capacity: a reclaimed task means its messages
  # become visible again, which is a delay rather than a failure. The webhook
  # receiver stays on guaranteed capacity.
  capacity_provider_strategy {
    capacity_provider = each.key == "worker" ? "FARGATE_SPOT" : "FARGATE"
    weight            = 1
  }

  # Lets `aws ecs execute-command` open a shell in a running container. Worth
  # having before the incident where you need it.
  enable_execute_command = true

  network_configuration {
    subnets         = var.private_subnet_ids
    security_groups = [aws_security_group.tasks.id]
    # Private subnets with a NAT gateway. A public IP here would make every
    # task directly reachable, which is what the load balancer exists to avoid.
    assign_public_ip = false
  }

  dynamic "load_balancer" {
    # A dynamic block generates zero or one of these depending on the service.
    # The worker gets none, which is how a resource can be shaped by data
    # instead of by a copied-and-edited second copy.
    for_each = each.value.public ? [1] : []

    content {
      target_group_arn = aws_lb_target_group.service[each.key].arn
      container_name   = each.key
      container_port   = each.value.port
    }
  }

  # A new task has to pass its health check for this long before it counts.
  # Too short and a slow starter is killed and replaced in a loop.
  health_check_grace_period_seconds = each.value.public ? 60 : null

  # Deploy two, keep at least one serving. With desired_count 2 this means a
  # rolling deploy that never drops below full capacity.
  deployment_maximum_percent         = 200
  deployment_minimum_healthy_percent = 50

  deployment_circuit_breaker {
    enable = true
    # A deployment whose tasks keep failing their health check rolls back on
    # its own. Without this, a bad image leaves the service trying forever
    # while the previous version is already gone.
    rollback = true
  }

  lifecycle {
    # Autoscaling owns this number at runtime. Without this, the next
    # `terraform apply` helpfully scales production back down to the number in
    # the code.
    ignore_changes = [desired_count]
  }

  depends_on = [aws_lb_listener.https]
}

# Scale the worker on the queue, not on CPU.
#
# CPU is a proxy for load; the number of messages waiting IS the load. A worker
# blocked on a slow database is idle and behind at the same time, which CPU
# scaling reads as "nothing to do".
resource "aws_appautoscaling_target" "worker" {
  service_namespace  = "ecs"
  resource_id        = "service/${aws_ecs_cluster.main.name}/${aws_ecs_service.main["worker"].name}"
  scalable_dimension = "ecs:service:DesiredCount"
  min_capacity       = 1
  max_capacity       = 10
}

resource "aws_appautoscaling_policy" "worker_queue_depth" {
  name               = "${local.name}-worker-queue-depth"
  policy_type        = "TargetTrackingScaling"
  service_namespace  = aws_appautoscaling_target.worker.service_namespace
  resource_id        = aws_appautoscaling_target.worker.resource_id
  scalable_dimension = aws_appautoscaling_target.worker.scalable_dimension

  target_tracking_scaling_policy_configuration {
    customized_metric_specification {
      metric_name = "ApproximateNumberOfMessagesVisible"
      namespace   = "AWS/SQS"
      statistic   = "Average"

      dimensions {
        name  = "QueueName"
        value = aws_sqs_queue.events.name
      }
    }

    # Roughly 100 waiting messages per worker.
    target_value = 100

    # Scale out fast, scale in slowly. Adding a worker costs pennies; removing
    # one mid-decision means its messages wait for a visibility timeout.
    scale_out_cooldown = 60
    scale_in_cooldown  = 300
  }
}
