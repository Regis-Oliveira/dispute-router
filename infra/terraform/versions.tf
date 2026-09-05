# Every Terraform project starts here, and the two blocks below are the ones
# worth understanding before anything else.

terraform {
  # Pin the tool. Terraform's own state format has changed between minor
  # versions, and a colleague running a newer version can write state an older
  # one refuses to read - so the version becomes a property of the project, not
  # of whoever ran it last.
  required_version = "~> 1.9"

  required_providers {
    aws = {
      source = "hashicorp/aws"
      # ~> 5.60 means ">= 5.60, < 6.0": patch and minor upgrades are welcome,
      # a major one is not. Providers are the code that actually talks to AWS,
      # and a major version renames and removes resource arguments.
      version = "~> 5.60"
    }
  }

  # State is the whole product.
  #
  # Terraform's job is to compare what you asked for with what already exists,
  # and it cannot ask AWS "what did I create last time?" - it has to remember.
  # That memory is the state file, and it is what makes `terraform destroy`
  # possible at all. A shell script can create things; only something with
  # state can update or remove exactly what it made.
  #
  # Which is also why the state does not live on a laptop. Remote state gives
  # the team one copy, and `use_lockfile` makes S3 refuse two simultaneous
  # applies - without which two people running apply at once produce a state
  # file describing neither of their changes.
  #
  # Commented out because it needs a bucket that exists first: the classic
  # chicken and egg, usually solved by creating that one bucket by hand, once.
  #
  # backend "s3" {
  #   bucket       = "dispute-router-tfstate"
  #   key          = "platform/terraform.tfstate"
  #   region       = "us-east-1"
  #   encrypt      = true
  #   use_lockfile = true
  # }
}

provider "aws" {
  region = var.region

  # Tags every taggable resource this provider creates. Worth setting on day
  # one: the question "what is this and who owns it?" arrives about six months
  # later, usually attached to a bill.
  default_tags {
    tags = {
      Project     = "dispute-router"
      Environment = var.environment
      ManagedBy   = "terraform"
    }
  }
}
