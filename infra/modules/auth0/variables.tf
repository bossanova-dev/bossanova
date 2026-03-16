variable "api_name" {
  description = "Auth0 API (resource server) display name"
  type        = string
  default     = "Bossanova API"
}

variable "api_audience" {
  description = "Auth0 API audience identifier"
  type        = string
}

variable "client_name" {
  description = "Auth0 application display name"
  type        = string
  default     = "Bossanova"
}

variable "callback_urls" {
  description = "Allowed callback URLs for PKCE flow"
  type        = list(string)
  default     = ["http://127.0.0.1/callback"]
}

variable "logout_urls" {
  description = "Allowed logout URLs"
  type        = list(string)
  default     = []
}

variable "web_origins" {
  description = "Allowed web origins (for SPA)"
  type        = list(string)
  default     = []
}

variable "enable_google_login" {
  description = "Enable Google social login"
  type        = bool
  default     = false
}

variable "google_client_id" {
  description = "Google OAuth client ID"
  type        = string
  default     = ""
}

variable "google_client_secret" {
  description = "Google OAuth client secret"
  type        = string
  default     = ""
  sensitive   = true
}

variable "enable_github_login" {
  description = "Enable GitHub social login"
  type        = bool
  default     = false
}

variable "github_client_id" {
  description = "GitHub OAuth app client ID"
  type        = string
  default     = ""
}

variable "github_client_secret" {
  description = "GitHub OAuth app client secret"
  type        = string
  default     = ""
  sensitive   = true
}
