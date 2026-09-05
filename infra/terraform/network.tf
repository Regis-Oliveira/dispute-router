# Security groups: the rules about who may talk to whom.
#
# Note that these reference each other rather than naming IP ranges. "Whatever
# is behind the load balancer's group" keeps being true when subnets change,
# when tasks move, and when somebody adds an availability zone; a CIDR block
# has to be remembered and updated.

resource "aws_security_group" "alb" {
  name        = "${local.name}-alb"
  description = "Public entry point"
  vpc_id      = var.vpc_id
}

resource "aws_vpc_security_group_ingress_rule" "alb_https" {
  security_group_id = aws_security_group.alb.id
  description       = "HTTPS from the internet"
  cidr_ipv4         = "0.0.0.0/0"
  from_port         = 443
  to_port           = 443
  ip_protocol       = "tcp"
}

resource "aws_vpc_security_group_egress_rule" "alb_to_tasks" {
  security_group_id            = aws_security_group.alb.id
  description                  = "To the services"
  referenced_security_group_id = aws_security_group.tasks.id
  ip_protocol                  = "-1"
}

resource "aws_security_group" "tasks" {
  name        = "${local.name}-tasks"
  description = "The three services"
  vpc_id      = var.vpc_id
}

# Only from the load balancer. Nothing else in the VPC can reach these ports,
# so a compromised instance elsewhere cannot call the ingest endpoint directly
# and skip whatever the load balancer is doing.
resource "aws_vpc_security_group_ingress_rule" "tasks_from_alb" {
  for_each = local.public_services

  security_group_id            = aws_security_group.tasks.id
  description                  = "${each.key} from the load balancer"
  referenced_security_group_id = aws_security_group.alb.id
  from_port                    = each.value.port
  to_port                      = each.value.port
  ip_protocol                  = "tcp"
}

# Outbound is wide open, which is worth being honest about rather than quiet.
# These tasks need Postgres, Redis, SQS, S3, Secrets Manager and ECR, and the
# AWS endpoints are public IPs that change. Narrowing this properly means VPC
# endpoints for each service - genuinely better, and a bigger piece of work than
# it looks.
resource "aws_vpc_security_group_egress_rule" "tasks_out" {
  security_group_id = aws_security_group.tasks.id
  description       = "To AWS services and the database"
  cidr_ipv4         = "0.0.0.0/0"
  ip_protocol       = "-1"
}
