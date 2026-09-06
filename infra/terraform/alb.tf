# The load balancer. Two services behind one, split by path.

resource "aws_lb" "main" {
  name               = local.name
  load_balancer_type = "application"
  subnets            = var.public_subnet_ids
  security_groups    = [aws_security_group.alb.id]

  # Refuses `terraform destroy` on this resource. The DNS name is what payment
  # processors have configured as their webhook endpoint, and a new load
  # balancer gets a new one - so destroying this is an outage that lasts until
  # every sender is reconfigured.
  enable_deletion_protection = var.environment == "production"

  # Longer than the slowest expected response. The CSV export streams tens of
  # thousands of rows, and the default 60s would cut it off mid-file with no
  # error the client can distinguish from a complete download.
  idle_timeout = 120

  drop_invalid_header_fields = true
}

resource "aws_lb_target_group" "service" {
  for_each = local.public_services

  name     = "${local.name}-${each.key}"
  port     = each.value.port
  protocol = "HTTP"
  vpc_id   = var.vpc_id
  # Fargate tasks have their own network interfaces, so the target is an IP
  # rather than an instance.
  target_type = "ip"

  health_check {
    path                = each.value.health_path
    interval            = 30
    timeout             = 5
    healthy_threshold   = 2
    unhealthy_threshold = 3
    matcher             = "200"
  }

  # How long to let in-flight requests finish before killing a draining task.
  # Shorter than the idle timeout above would cut off the export this exists
  # to protect.
  deregistration_delay = 30

  lifecycle {
    # AWS caps target group names at 32 characters, and
    # "dispute-router-production-ingest" is exactly 32 - it plans, applies, and
    # leaves the next person one character of headroom.
    #
    # Without this the failure arrives at apply, from AWS, phrased as a
    # complaint about the name rather than about its length, and only after
    # some of the stack already exists. A precondition moves it to plan time
    # and says what to do about it.
    precondition {
      condition     = length("${local.name}-${each.key}") <= 32
      error_message = "Target group name '${local.name}-${each.key}' is ${length("${local.name}-${each.key}")} characters; AWS allows 32. Shorten the environment or service name."
    }
  }
}

resource "aws_lb_listener" "https" {
  load_balancer_arn = aws_lb.main.arn
  port              = 443
  protocol          = "HTTPS"

  # A modern policy, and a real decision: it drops TLS 1.0 and 1.1. Some
  # payment processors still sign webhooks from old stacks, so this is worth
  # confirming against their documentation rather than assuming.
  ssl_policy = "ELBSecurityPolicy-TLS13-1-2-2021-06"

  # Certificate ARN comes from ACM, managed with the domain rather than here.
  certificate_arn = var.certificate_arn

  # Anything that matches no rule below. 404 rather than a default service, so
  # a mistyped path is obviously wrong instead of quietly reaching something.
  default_action {
    type = "fixed-response"

    fixed_response {
      content_type = "application/json"
      message_body = jsonencode({ error = "not found" })
      status_code  = "404"
    }
  }
}

resource "aws_lb_listener_rule" "ingest" {
  listener_arn = aws_lb_listener.https.arn
  priority     = 10

  action {
    type             = "forward"
    target_group_arn = aws_lb_target_group.service["ingest"].arn
  }

  condition {
    path_pattern {
      values = ["/webhooks/*"]
    }
  }
}

resource "aws_lb_listener_rule" "api" {
  listener_arn = aws_lb_listener.https.arn
  priority     = 20

  action {
    type             = "forward"
    target_group_arn = aws_lb_target_group.service["api"].arn
  }

  condition {
    path_pattern {
      values = ["/api/*"]
    }
  }
}
