output "r2_bucket_name" {
  description = "R2 bucket name for Litestream config"
  value       = cloudflare_r2_bucket.litestream.name
}

output "api_fqdn" {
  description = "Fully qualified domain name for the API"
  value       = var.domain != "" ? "${var.api_subdomain}.${var.domain}" : ""
}
