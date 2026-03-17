variable "account_id" {
  description = "Cloudflare account ID"
  type        = string
}

variable "zone_id" {
  description = "Cloudflare DNS zone ID"
  type        = string
  default     = ""
}

variable "domain" {
  description = "Domain name (e.g. bossanova.dev)"
  type        = string
  default     = ""
}

variable "api_subdomain" {
  description = "API subdomain (e.g. api)"
  type        = string
  default     = "api"
}

variable "fly_hostname" {
  description = "Fly.io app hostname to CNAME to"
  type        = string
  default     = ""
}

variable "r2_bucket_name" {
  description = "R2 bucket name for Litestream backups"
  type        = string
}

variable "r2_location" {
  description = "R2 bucket location hint"
  type        = string
  default     = "wnam"
}

variable "pages_project_name" {
  description = "Cloudflare Pages project name"
  type        = string
}

variable "pages_custom_domain" {
  description = "Custom domain for CF Pages (e.g. app.bossanova.dev)"
  type        = string
}
