# GitHub module — GitHub App with webhook secret for VCS event delivery.

terraform {
  required_providers {
    github = {
      source  = "integrations/github"
      version = "~> 6.0"
    }
  }
}

# Webhook secret for HMAC verification.
resource "random_password" "webhook_secret" {
  length  = 32
  special = false
}

# GitHub App configuration (created manually, configured here).
# Note: GitHub Apps cannot be created via Terraform — this manages
# the webhook endpoint and secret after manual app creation.
resource "github_app_installation_repositories" "repos" {
  count = length(var.installation_id) > 0 ? 1 : 0

  installation_id    = var.installation_id
  selected_repositories = var.selected_repositories
}
