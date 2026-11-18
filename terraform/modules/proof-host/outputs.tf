output "base_url" {
  description = "Origin the proof pages are served from; a publisher's proof base URL."
  value       = "https://${cloudflare_workers_domain.proof.hostname}"
}

output "bucket_name" {
  description = "Name of the private R2 bucket that holds the proof pages."
  value       = cloudflare_r2_bucket.proof.name
}

output "access_aud" {
  description = "AUD tag of the Access application on /_signin and /_publish. Not a secret."
  value       = cloudflare_zero_trust_access_application.proof.aud
}

output "publish_client_id" {
  description = "Client ID of the Access service token that may upload proof pages."
  value       = cloudflare_zero_trust_access_service_token.publish.client_id
}

output "publish_client_secret" {
  description = "Client secret of the Access service token that may upload proof pages."
  sensitive   = true
  value       = cloudflare_zero_trust_access_service_token.publish.client_secret
}
