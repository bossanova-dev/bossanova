terraform {
  required_version = ">= 1.5"

  backend "s3" {
    bucket = "bossanova-terraform"
    key    = "staging/terraform.tfstate"
    region = "us-west-2"
  }
}

module "fly" {
  source = "../../modules/fly"

  app_name      = "bosso-staging"
  org           = var.fly_org
  region        = var.fly_region
  image         = var.bosso_image
  oidc_issuer   = module.auth0.issuer
  oidc_audience = module.auth0.audience
}

module "auth0" {
  source = "../../modules/auth0"

  api_audience = "https://api.staging.bossanova.dev"
  client_name  = "Bossanova Staging"

  callback_urls = [
    "http://127.0.0.1/callback",
  ]

  web_origins = var.web_origins
}

module "cloudflare" {
  source = "../../modules/cloudflare"

  account_id     = var.cloudflare_account_id
  zone_id        = var.cloudflare_zone_id
  domain         = var.domain
  api_subdomain  = "api.staging"
  fly_hostname   = module.fly.app_hostname
  r2_bucket_name = "bosso-litestream-staging"
}

module "github" {
  source = "../../modules/github"

  webhook_url = "https://${module.fly.app_hostname}/webhook"
}
