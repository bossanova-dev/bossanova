# Root Terraform config — not applied directly.
# Use infra/environments/ with terraform workspaces (staging/production).
#
# This file documents the overall module composition.

terraform {
  required_version = ">= 1.5"
}
