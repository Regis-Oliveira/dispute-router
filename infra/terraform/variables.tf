# Variables are the inputs. Anything that differs between staging and
# production belongs here; anything that does not, does not.

variable "region" {
  description = "AWS region to deploy into."
  type        = string
  default     = "us-east-1"
}

variable "environment" {
  description = "Environment name, used in resource names and tags."
  type        = string
  default     = "staging"

  # Validation runs at plan time, so a typo fails before anything is created
  # rather than after half of it is.
  validation {
    condition     = contains(["staging", "production"], var.environment)
    error_message = "environment must be staging or production."
  }
}

variable "vpc_id" {
  description = "Existing VPC to deploy into. Networking is deliberately not created here - see the README."
  type        = string
}

variable "private_subnet_ids" {
  description = "Subnets for the tasks. Private: nothing here needs a public IP."
  type        = list(string)

  validation {
    condition     = length(var.private_subnet_ids) >= 2
    error_message = "Give at least two subnets in different availability zones, or one AZ failing takes the platform with it."
  }
}

variable "public_subnet_ids" {
  description = "Subnets for the load balancer, which does need to be reachable."
  type        = list(string)
}

variable "database_url_secret_arn" {
  description = "Secrets Manager ARN holding the Postgres connection string. Created outside this stack with the database itself."
  type        = string
}

variable "redis_url" {
  description = "ElastiCache endpoint. Not a secret - it is a hostname behind a security group."
  type        = string
}

variable "image_tag" {
  description = "Image tag to deploy. A git sha, never 'latest' - see the README."
  type        = string

  validation {
    condition     = var.image_tag != "latest"
    error_message = "Deploy an immutable tag. 'latest' means the running version is whatever was pushed most recently, which is unknowable during an incident."
  }
}

variable "dashboard_origin" {
  description = "Origin allowed to call the API and upload evidence, for CORS."
  type        = string
  default     = "https://disputes.example.com"
}

variable "log_retention_days" {
  description = "How long to keep container logs. CloudWatch bills for storage forever by default."
  type        = number
  default     = 30
}

variable "alarm_topic_arn" {
  description = "SNS topic alarms publish to. Empty disables alarm actions, which is fine for a first apply."
  type        = string
  default     = ""
}

variable "certificate_arn" {
  description = "ACM certificate for the load balancer. Managed with the domain, not here."
  type        = string
}
