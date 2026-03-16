variable "installation_id" {
  description = "GitHub App installation ID"
  type        = string
  default     = ""
}

variable "selected_repositories" {
  description = "List of repository names to install the app on"
  type        = list(string)
  default     = []
}

variable "webhook_url" {
  description = "Webhook delivery URL (orchestrator endpoint)"
  type        = string
  default     = ""
}
