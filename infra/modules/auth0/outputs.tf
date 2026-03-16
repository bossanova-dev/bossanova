output "client_id" {
  description = "Auth0 application client ID (for BOSS_OIDC_CLIENT_ID)"
  value       = auth0_client.app.client_id
}

output "issuer" {
  description = "Auth0 issuer URL (for BOSSO_OIDC_ISSUER)"
  value       = "https://${auth0_client.app.id}.us.auth0.com/"
}

output "audience" {
  description = "Auth0 API audience (for BOSSO_OIDC_AUDIENCE)"
  value       = auth0_resource_server.api.identifier
}
