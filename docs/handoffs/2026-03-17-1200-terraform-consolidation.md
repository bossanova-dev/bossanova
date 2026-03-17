## Handoff: Terraform Consolidation (Flight Leg 3)

**Date:** 2026-03-17
**Branch:** main
**Flight ID:** fp-2026-03-17-production-readiness-gaps
**Planning Doc:** docs/plans/2026-03-17-production-readiness-gaps.md

### Tasks Completed

- bossanova-bbc2: Add CF Pages project to cloudflare Terraform module
- bossanova-ldes: Consolidate Terraform envs into single workspace-keyed config
- bossanova-880l: Add \*.tfvars to .gitignore
- bossanova-8y9w: [HANDOFF] Review Flight Leg 3

### Files Changed

- `infra/modules/cloudflare/main.tf:20-34` — Added `cloudflare_pages_project` and `cloudflare_pages_domain` resources for web SPA hosting
- `infra/modules/cloudflare/variables.tf:41-50` — Added `pages_project_name` and `pages_custom_domain` variables
- `infra/environments/main.tf:1-96` — Created; single workspace-keyed config with locals map for staging/production, all four module calls (fly, auth0, cloudflare, github), derived domains with hyphenated suffix for CF Universal SSL
- `infra/environments/variables.tf:1-5` — Created; only sensitive secrets (`google_client_secret`, `github_client_secret`, `github_app_installation_id`)
- `infra/environments/staging/` — Deleted (merged into workspace-keyed config)
- `infra/environments/production/` — Deleted (merged into workspace-keyed config)
- `infra/main.tf:2` — Updated comment to reflect workspace approach
- `.gitignore:25-27` — Added `*.tfvars` and `!*.tfvars.example`

### Learnings & Notes

- The consolidated config uses `terraform.workspace` as the key into a `local.config` map — `local.c = local.config[local.env]` for clean access
- Domain scheme uses hyphenated suffixes (`orchestrator-staging`, `app-staging`) for CF Universal SSL compatibility (only covers `*.bossanova.dev`, one level)
- Auth0 uses a single tenant (`id.bossanova.dev`) shared across envs with separate applications per env
- `deploy.yml` bump-versions job already targeted `infra/environments/main.tf` — no changes needed
- `cloudflare_account_id` and `cloudflare_zone_id` have placeholder TODO values — will be filled in when applying for the first time
- The S3 backend key changed from per-env paths (`staging/terraform.tfstate`, `production/terraform.tfstate`) to a single `terraform.tfstate` — workspaces handle state separation

### Issues Encountered

- None — implementation straightforward

### Current Status

- Tests: pass (all Go modules green, unchanged)
- Lint: pass (go vet clean)
- Format: pass (prettier reformatting committed)
- All 8 structural verification checks passed (module wiring, variable declarations, workspace keying, directory cleanup, gitignore, deploy.yml targeting)

### Next Steps

This is the **final flight leg** of the production readiness gaps plan. All 10 tasks across 3 flight legs are now complete:

- Flight Leg 1: Container Infrastructure (Dockerfile, fly.toml, litestream.yml, db.go fix)
- Flight Leg 2: CI/CD Workflows (test-web.yml, deploy.yml, release.yml deletion)
- Flight Leg 3: Terraform Consolidation (CF Pages module, workspace-keyed config, gitignore)

### Resume Command

This flight plan is complete. No further resume needed.
