# One registry per service.
#
# for_each over the same map that defines the services, so a fourth service
# cannot end up without somewhere to push its image.
resource "aws_ecr_repository" "service" {
  for_each = local.services

  name = "${local.name}/${each.key}"

  # Refuse to overwrite an existing tag. This is what makes a git sha actually
  # mean something: without it, someone can push a different image under the tag
  # that is already running, and the version you think is deployed is not the
  # one that is.
  image_tag_mutability = "IMMUTABLE"

  image_scanning_configuration {
    scan_on_push = true
  }
}

# Registries grow forever and are billed by the gigabyte.
resource "aws_ecr_lifecycle_policy" "service" {
  for_each = aws_ecr_repository.service

  repository = each.value.name

  policy = jsonencode({
    rules = [{
      rulePriority = 1
      description  = "Keep the last 30 images"
      selection = {
        tagStatus   = "any"
        countType   = "imageCountMoreThan"
        countNumber = 30
      }
      action = { type = "expire" }
    }]
  })
}
