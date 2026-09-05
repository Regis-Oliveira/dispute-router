# Data sources read things Terraform did not create.
#
# The distinction matters: a `resource` is something this project owns and will
# change or destroy; a `data` source is something it only looks at. Reading the
# account id rather than hardcoding it is what lets the same code apply to
# staging and production without an edit.

data "aws_caller_identity" "current" {}

# The network is deliberately not created here.
#
# A VPC, its subnets, route tables, NAT gateways and endpoints are a different
# lifecycle from an application: they change rarely, they are shared by
# everything in the account, and destroying them by accident takes down services
# this project has never heard of. They belong in their own state.
#
# This is the general shape of the argument for splitting Terraform up: not by
# size, but by blast radius and by how often things change.
data "aws_vpc" "main" {
  id = var.vpc_id
}
