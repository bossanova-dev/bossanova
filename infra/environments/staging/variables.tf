variable "fly_org" {
  description = "Fly.io organization slug"
  type        = string
}

variable "fly_region" {
  description = "Fly.io deployment region"
  type        = string
  default     = "sjc"
}

variable "bosso_image" {
  description = "Docker image for bosso"
  type        = string
}

variable "cloudflare_account_id" {
  description = "Cloudflare account ID"
  type        = string
}

variable "cloudflare_zone_id" {
  description = "Cloudflare DNS zone ID"
  type        = string
  default     = ""
}

variable "domain" {
  description = "Base domain (e.g. bossanova.dev)"
  type        = string
  default     = ""
}

variable "web_origins" {
  description = "Allowed web origins for Auth0 SPA"
  type        = list(string)
  default     = []
}
