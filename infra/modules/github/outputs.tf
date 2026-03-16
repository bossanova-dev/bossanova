output "webhook_secret" {
  description = "Generated webhook secret for HMAC verification"
  value       = random_password.webhook_secret.result
  sensitive   = true
}
