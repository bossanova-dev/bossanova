/**
 * Proof host module
 *
 * Creates:
 *   - A private R2 bucket holding per-PR proof pages and their media
 *   - The Worker that serves them, verifying the Cloudflare Access JWT itself,
 *     and its custom domain (which creates its own proxied DNS record)
 *   - A daily cron trigger for the Worker's retention sweep
 *   - An Access application on the hostname's /_signin and /_publish paths only,
 *     with an email-domain policy for readers and a service token for uploads
 *
 * The bucket gets no r2.dev address and no public access: only the Worker's
 * R2 binding reads or writes it. Page reads reach the Worker with no Access in
 * front, and it checks the CF_Authorization cookie itself, so a page framed
 * with an expired session shows the Worker's frameable sign-in gate instead of
 * Access's login redirect, which refuses to be framed. That cookie must reach
 * the Worker from a third-party iframe, so SameSite is none.
 */

locals {
  name = coalesce(var.name, var.bucket_name)
}

resource "cloudflare_r2_bucket" "proof" {
  account_id = var.account_id
  location   = var.location == null ? null : upper(var.location)
  name       = var.bucket_name
}

resource "cloudflare_zero_trust_access_application" "proof" {
  account_id                 = var.account_id
  allowed_idps               = length(var.identity_provider_ids) == 0 ? null : var.identity_provider_ids
  app_launcher_visible       = false
  auto_redirect_to_identity  = false
  domain                     = "${var.hostname}/_signin"
  name                       = "${local.name} sign-in and publish"
  same_site_cookie_attribute = "none"
  self_hosted_domains        = ["${var.hostname}/_signin", "${var.hostname}/_publish"]
  session_duration           = var.session_duration
  type                       = "self_hosted"
}

resource "cloudflare_zero_trust_access_policy" "readers" {
  account_id     = var.account_id
  application_id = cloudflare_zero_trust_access_application.proof.id
  decision       = "allow"
  name           = "Allow ${join(", ", var.allowed_email_domains)} users"
  precedence     = 1

  include {
    email_domain = var.allowed_email_domains
  }
}

resource "cloudflare_zero_trust_access_service_token" "publish" {
  account_id = var.account_id
  name       = "${local.name} publisher"
}

resource "cloudflare_zero_trust_access_policy" "publisher" {
  account_id     = var.account_id
  application_id = cloudflare_zero_trust_access_application.proof.id
  decision       = "non_identity"
  name           = "${local.name} publisher"
  precedence     = 2

  include {
    service_token = [cloudflare_zero_trust_access_service_token.publish.id]
  }
}

resource "cloudflare_workers_script" "proof" {
  account_id         = var.account_id
  compatibility_date = "2025-09-01"
  content            = file("${path.module}/worker.mjs")
  module             = true
  name               = local.name

  r2_bucket_binding {
    bucket_name = cloudflare_r2_bucket.proof.name
    name        = "PROOF_BUCKET"
  }

  plain_text_binding {
    name = "ACCESS_AUD"
    text = cloudflare_zero_trust_access_application.proof.aud
  }

  plain_text_binding {
    name = "ACCESS_TEAM_DOMAIN"
    text = var.access_team_domain
  }

  plain_text_binding {
    name = "ALLOWED_EMAIL_DOMAINS"
    text = join(",", var.allowed_email_domains)
  }

  plain_text_binding {
    name = "PUBLISH_CLIENT_ID"
    text = cloudflare_zero_trust_access_service_token.publish.client_id
  }

  plain_text_binding {
    name = "FRAME_ANCESTORS"
    text = join(" ", var.frame_ancestors)
  }

  plain_text_binding {
    name = "RETENTION_DAYS"
    text = tostring(var.retention_days)
  }
}

resource "cloudflare_workers_domain" "proof" {
  account_id = var.account_id
  hostname   = var.hostname
  service    = cloudflare_workers_script.proof.name
  zone_id    = var.zone_id
}

resource "cloudflare_workers_cron_trigger" "proof" {
  account_id  = var.account_id
  script_name = cloudflare_workers_script.proof.name
  schedules   = ["17 3 * * *"]
}
