variable "app_name" {
  description = "Fly.io application name"
  type        = string
}

variable "org" {
  description = "Fly.io organization slug"
  type        = string
}

variable "region" {
  description = "Fly.io region (e.g. sjc, iad)"
  type        = string
  default     = "sjc"
}

variable "image" {
  description = "Docker image for the bosso binary"
  type        = string
}

variable "cpus" {
  description = "Number of shared CPUs"
  type        = number
  default     = 1
}

variable "memory_mb" {
  description = "Memory in MB"
  type        = number
  default     = 256
}

variable "volume_size_gb" {
  description = "Persistent volume size in GB"
  type        = number
  default     = 1
}

variable "oidc_issuer" {
  description = "OIDC issuer URL (Auth0)"
  type        = string
}

variable "oidc_audience" {
  description = "OIDC audience (API identifier)"
  type        = string
}
