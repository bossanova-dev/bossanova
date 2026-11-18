variable "account_id" {
  description = "Cloudflare account ID that owns the bucket, the Worker and the Access application."
  type        = string
}

variable "zone_id" {
  description = "Cloudflare zone ID of the zone that holds hostname."
  type        = string
}

variable "hostname" {
  description = "Hostname the Worker serves proof pages on, such as proof.example.com. The Worker custom domain creates its proxied DNS record."
  type        = string

  validation {
    condition     = can(regex("^[a-z0-9-]+(\\.[a-z0-9-]+)+$", var.hostname))
    error_message = "hostname must be a bare lower-case hostname such as proof.example.com, with no scheme, port or path."
  }
}

variable "bucket_name" {
  description = "Name of the private R2 bucket that holds the proof pages."
  type        = string

  validation {
    condition     = can(regex("^[a-z0-9][a-z0-9-]{1,61}[a-z0-9]$", var.bucket_name))
    error_message = "bucket_name must be 3 to 63 lower-case letters, digits or hyphens, starting and ending with a letter or digit."
  }
}

variable "location" {
  default     = null
  description = "R2 location hint for the bucket: APAC, EEUR, ENAM, WEUR or WNAM. Null lets Cloudflare choose."
  type        = string

  validation {
    condition     = contains(["APAC", "EEUR", "ENAM", "WEUR", "WNAM"], upper(coalesce(var.location, "APAC")))
    error_message = "location must be one of APAC, EEUR, ENAM, WEUR or WNAM, or null."
  }
}

variable "access_team_domain" {
  description = "Cloudflare Access team domain that issues the JWTs, without a scheme, such as example.cloudflareaccess.com."
  type        = string

  validation {
    condition     = can(regex("^[A-Za-z0-9-]+(\\.[A-Za-z0-9-]+)+$", var.access_team_domain))
    error_message = "access_team_domain must be a bare hostname such as example.cloudflareaccess.com, with no scheme or path."
  }
}

variable "allowed_email_domains" {
  description = "Email domains whose SSO logins may read proof pages. Access admits them at /_signin and the Worker checks every page read against the same list."
  type        = list(string)

  validation {
    condition = length(var.allowed_email_domains) > 0 && alltrue([
      for domain in var.allowed_email_domains : can(regex("^[A-Za-z0-9-]+(\\.[A-Za-z0-9-]+)+$", domain))
    ])
    error_message = "allowed_email_domains must list at least one bare domain such as example.com."
  }
}

variable "identity_provider_ids" {
  default     = []
  description = "Cloudflare Access identity provider IDs allowed at /_signin. Empty allows every identity provider on the account."
  type        = list(string)
}

variable "session_duration" {
  default     = "24h"
  description = "How long an Access session lasts before the reader signs in again, such as 24h."
  type        = string
}

variable "frame_ancestors" {
  default     = ["https://app.bossanova.dev", "https://app-staging.bossanova.dev"]
  description = "HTTPS origins allowed to frame a proof page. Empty forbids framing."
  type        = list(string)

  validation {
    condition = alltrue([
      for origin in var.frame_ancestors : can(regex("^https://[a-z0-9.-]+(:[0-9]+)?$", origin))
    ])
    error_message = "Each frame_ancestors entry must be a lower-case https origin with no path, such as https://app.example.com."
  }
}

variable "retention_days" {
  default     = 90
  description = "Days a proof object is kept; a daily sweep deletes anything uploaded earlier."
  type        = number

  validation {
    condition     = var.retention_days >= 1 && floor(var.retention_days) == var.retention_days
    error_message = "Retention days must be a positive integer."
  }
}

variable "name" {
  default     = null
  description = "Name of the Worker, and the prefix of the Access application and service token names. Null uses bucket_name."
  type        = string

  validation {
    condition     = var.name == null || can(regex("^[a-z0-9][a-z0-9-]{0,62}$", var.name))
    error_message = "name must be up to 63 lower-case letters, digits or hyphens, starting with a letter or digit."
  }
}
