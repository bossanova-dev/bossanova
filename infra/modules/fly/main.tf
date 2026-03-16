# Fly.io module — deploys the bosso orchestrator.

terraform {
  required_providers {
    fly = {
      source  = "fly-apps/fly"
      version = "~> 0.1"
    }
  }
}

resource "fly_app" "bosso" {
  name = var.app_name
  org  = var.org
}

resource "fly_volume" "data" {
  app    = fly_app.bosso.name
  name   = "bosso_data"
  size   = var.volume_size_gb
  region = var.region
}

resource "fly_machine" "bosso" {
  app    = fly_app.bosso.name
  name   = "${var.app_name}-machine"
  region = var.region

  image = var.image

  cpus     = var.cpus
  memorymb = var.memory_mb
  cputype  = "shared"

  mounts = [
    {
      volume = fly_volume.data.id
      path   = "/data"
    }
  ]

  env = {
    BOSSO_ADDR          = ":8080"
    BOSSO_DB_PATH       = "/data/bosso.db"
    BOSSO_OIDC_ISSUER   = var.oidc_issuer
    BOSSO_OIDC_AUDIENCE = var.oidc_audience
  }

  services = [
    {
      internal_port = 8080
      protocol      = "tcp"
      ports = [
        {
          port     = 443
          handlers = ["tls", "http"]
        }
      ]
    }
  ]
}
