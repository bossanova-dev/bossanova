# Cloudflare module — R2 bucket for Litestream backups, DNS records.

terraform {
  required_providers {
    cloudflare = {
      source  = "cloudflare/cloudflare"
      version = "~> 4.0"
    }
  }
}

# R2 bucket for Litestream SQLite replication.
resource "cloudflare_r2_bucket" "litestream" {
  account_id = var.account_id
  name       = var.r2_bucket_name
  location   = var.r2_location
}

# DNS CNAME record pointing to Fly.io app.
resource "cloudflare_record" "api" {
  count = var.domain != "" ? 1 : 0

  zone_id = var.zone_id
  name    = var.api_subdomain
  content = var.fly_hostname
  type    = "CNAME"
  proxied = true
}

# Cloudflare Pages project for web SPA.
resource "cloudflare_pages_project" "web" {
  account_id        = var.account_id
  name              = var.pages_project_name
  production_branch = "main"
}

# Custom domain for CF Pages.
resource "cloudflare_pages_domain" "web" {
  account_id   = var.account_id
  project_name = cloudflare_pages_project.web.name
  domain       = var.pages_custom_domain
}
