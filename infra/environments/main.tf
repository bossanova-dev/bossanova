terraform {
  required_version = ">= 1.5"

  backend "s3" {
    bucket = "bossanova-terraform"
    key    = "terraform.tfstate"
    region = "us-west-2"
  }
}

locals {
  env    = terraform.workspace
  domain = "bossanova.dev"

  # Hyphenated suffix for non-production: "orchestrator-staging" vs "orchestrator"
  suffix = local.env == "production" ? "" : "-${local.env}"

  # Non-sensitive infrastructure IDs (safe to commit)
  fly_org               = "recurser"
  cloudflare_account_id = "your-account-id" # TODO: fill in real value
  cloudflare_zone_id    = "your-zone-id"    # TODO: fill in real value

  config = {
    staging = {
      fly_app_name       = "bosso-staging"
      fly_region         = "sjc"
      fly_cpus           = 1
      fly_memory_mb      = 256
      fly_volume_size_gb = 1
      bosso_image        = "registry.fly.io/bosso-staging:v0.1.0" # updated by deploy workflow
      pages_project_name = "bossanova-web-staging"
      auth0_client_name  = "Bossanova Staging"
      r2_bucket_name     = "bosso-litestream-staging"
    }
    production = {
      fly_app_name       = "bosso"
      fly_region         = "sjc"
      fly_cpus           = 2
      fly_memory_mb      = 512
      fly_volume_size_gb = 2
      bosso_image        = "registry.fly.io/bosso:v0.1.0" # updated by deploy workflow
      pages_project_name = "bossanova-web"
      auth0_client_name  = "Bossanova"
      r2_bucket_name     = "bosso-litestream"
    }
  }

  c = local.config[local.env]

  # Derived domains (hyphenated for CF Universal SSL compatibility)
  api_subdomain = "orchestrator${local.suffix}"            # orchestrator | orchestrator-staging
  api_domain    = "${local.api_subdomain}.${local.domain}" # orchestrator.bossanova.dev
  app_domain    = "app${local.suffix}.${local.domain}"     # app.bossanova.dev | app-staging.bossanova.dev
  api_audience  = "https://${local.api_domain}"

  # Auth0: single tenant shared across all envs, separate apps per env
  auth0_issuer = "https://id.${local.domain}/"

  cors_origins  = ["https://${local.app_domain}"]
  web_origins   = ["https://${local.app_domain}"]
  callback_urls = ["http://127.0.0.1/callback"]
  logout_urls   = ["https://${local.app_domain}"]
}

module "fly" {
  source         = "../modules/fly"
  app_name       = local.c.fly_app_name
  org            = local.fly_org
  region         = local.c.fly_region
  image          = local.c.bosso_image
  cpus           = local.c.fly_cpus
  memory_mb      = local.c.fly_memory_mb
  volume_size_gb = local.c.fly_volume_size_gb
  oidc_issuer    = local.auth0_issuer # same issuer for all envs
  oidc_audience  = local.api_audience # env-specific audience
}

module "auth0" {
  source        = "../modules/auth0"
  api_audience  = local.api_audience
  client_name   = local.c.auth0_client_name
  callback_urls = local.callback_urls
  web_origins   = local.web_origins
  logout_urls   = local.logout_urls
}

module "cloudflare" {
  source              = "../modules/cloudflare"
  account_id          = local.cloudflare_account_id
  zone_id             = local.cloudflare_zone_id
  domain              = local.domain
  api_subdomain       = local.api_subdomain
  fly_hostname        = module.fly.app_hostname
  r2_bucket_name      = local.c.r2_bucket_name
  pages_project_name  = local.c.pages_project_name
  pages_custom_domain = local.app_domain
}

module "github" {
  source          = "../modules/github"
  installation_id = var.github_app_installation_id
  webhook_url     = "https://${local.api_domain}/webhooks/github"
}
