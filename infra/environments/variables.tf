# No variables needed — all non-sensitive config is in locals.
# Sensitive secrets (social login) are passed via TF_VAR_* env vars if needed.
variable "google_client_secret" { type = string; default = ""; sensitive = true }
variable "github_client_secret" { type = string; default = ""; sensitive = true }
variable "github_app_installation_id" { type = string; default = "" }
