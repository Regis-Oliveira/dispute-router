locals {
  name = "dispute-router-${var.environment}"

  # The three services, described once.
  #
  # Everything that differs between them lives in this map; everything shared
  # is written once below. Adding a fourth service becomes an entry here rather
  # than a copied block - and a copied block is where the drift starts.
  services = {
    ingest = {
      # Receives webhooks. Small and latency-sensitive: a payment processor
      # will retry, but it will also mark the endpoint unhealthy.
      cpu           = 512
      memory        = 1024
      desired_count = 2
      port          = 8080
      public        = true
      health_path   = "/healthz"
    }

    api = {
      # Serves the dashboard. Read-only, so it can scale independently of the
      # write path - which is the whole reason it is a separate service.
      cpu           = 512
      memory        = 1024
      desired_count = 2
      port          = 8081
      public        = true
      health_path   = "/healthz"
    }

    worker = {
      # Drains the deadline queue. Nothing connects to it, so it has no port,
      # no target group and no inbound rule at all.
      cpu           = 256
      memory        = 512
      desired_count = 1
      port          = 0
      public        = false
      health_path   = ""
    }
  }

  # Services that are actually reachable. Used to build target groups and
  # listener rules without writing "if this one" three times.
  public_services = { for name, cfg in local.services : name => cfg if cfg.public }
}
