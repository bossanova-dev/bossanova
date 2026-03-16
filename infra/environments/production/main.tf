terraform {
  required_version = ">= 1.5"

  backend "s3" {
    bucket = "bossanova-terraform"
    key    = "production/terraform.tfstate"
    region = "us-west-2"
  }
}

module "fly" {
  source = "../../modules/fly"

  app_name       = "bosso"
  org            = var.fly_org
  region         = var.fly_region
  image          = var.bosso_image
  cpus           = 2
  memory_mb      = 512
  volume_size_gb = 2
  oidc_issuer    = module.auth0.issuer
  oidc_audience  = module.auth0.audience
}

module "auth0" {
  source = "../../modules/auth0"

  api_audience = "https://api.bossanova.dev"
  client_name  = "Bossanova"

  callback_urls = [
    "http://127.0.0.1/callback",
  ]

  web_origins = var.web_origins
  logout_urls = var.logout_urls
}

module "cloudflare" {
  source = "../../modules/cloudflare"

  account_id     = var.cloudflare_account_id
  zone_id        = var.cloudflare_zone_id
  domain         = var.domain
  api_subdomain  = "api"
  fly_hostname   = module.fly.app_hostname
  r2_bucket_name = "bosso-litestream-production"
}

module "github" {
  source = "../../modules/github"

  installation_id = var.github_app_installation_id
  webhook_url     = "https://api.${var.domain}/webhook"
}
