# Auth0 module — configures tenant, SPA app, API audience, connections.

terraform {
  required_providers {
    auth0 = {
      source  = "auth0/auth0"
      version = "~> 1.0"
    }
  }
}

# API (resource server) — the audience for JWT validation.
resource "auth0_resource_server" "api" {
  name       = var.api_name
  identifier = var.api_audience

  signing_alg                                    = "RS256"
  skip_consent_for_verifiable_first_party_clients = true
  token_lifetime                                  = 86400 # 24 hours

  scopes {
    value       = "openid"
    description = "OpenID Connect"
  }
  scopes {
    value       = "profile"
    description = "User profile"
  }
  scopes {
    value       = "email"
    description = "Email address"
  }
  scopes {
    value       = "offline_access"
    description = "Refresh token"
  }
}

# Native/SPA application — used by boss CLI (PKCE) and web SPA.
resource "auth0_client" "app" {
  name     = var.client_name
  app_type = "native"

  callbacks          = var.callback_urls
  allowed_logout_urls = var.logout_urls
  web_origins         = var.web_origins

  jwt_configuration {
    alg = "RS256"
  }

  grant_types = [
    "authorization_code",
    "refresh_token",
  ]

  oidc_conformant = true

  refresh_token {
    rotation_type   = "rotating"
    expiration_type = "expiring"
    idle_token_lifetime         = 1296000 # 15 days
    token_lifetime              = 2592000 # 30 days
    leeway                      = 0
    infinite_idle_token_lifetime = false
    infinite_token_lifetime      = false
  }
}

# Database connection (email/password).
resource "auth0_connection" "database" {
  name     = "Username-Password-Authentication"
  strategy = "auth0"

  enabled_clients = [
    auth0_client.app.id,
  ]

  options {
    password_policy        = "good"
    brute_force_protection = true
  }
}

# Google social login (optional).
resource "auth0_connection" "google" {
  count    = var.enable_google_login ? 1 : 0
  name     = "google-oauth2"
  strategy = "google-oauth2"

  enabled_clients = [
    auth0_client.app.id,
  ]

  options {
    client_id     = var.google_client_id
    client_secret = var.google_client_secret
    scopes        = ["openid", "profile", "email"]
  }
}

# GitHub social login (optional).
resource "auth0_connection" "github" {
  count    = var.enable_github_login ? 1 : 0
  name     = "github"
  strategy = "github"

  enabled_clients = [
    auth0_client.app.id,
  ]

  options {
    client_id     = var.github_client_id
    client_secret = var.github_client_secret
    scopes        = ["openid", "profile", "email"]
  }
}
