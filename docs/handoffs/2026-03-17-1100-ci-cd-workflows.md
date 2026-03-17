## Handoff: CI/CD Workflows (Flight Leg 2)

**Date:** 2026-03-17
**Branch:** main
**Flight ID:** fp-2026-03-17-production-readiness-gaps
**Planning Doc:** docs/plans/2026-03-17-production-readiness-gaps.md

### Tasks Completed

- bossanova-el11: Create web SPA CI workflow (test-web.yml)
- bossanova-q1hm: Create tag-triggered deploy workflow (deploy.yml)
- bossanova-l12x: Delete release.yml (merged into deploy.yml)

### Files Changed

- `.github/workflows/test-web.yml:1-68` — Created; dorny/paths-filter two-job pattern for web SPA, triggers on services/web/** and proto/**, runs npm ci + lint + build
- `.github/workflows/deploy.yml:1-161` — Created; tag-triggered (v*) with five jobs: deploy-bosso (Docker build+push+fly deploy), deploy-web (npm build+CF Pages deploy), build (cross-platform matrix), release (GitHub Release), bump-versions (sed update bosso_image in TF)
- `.github/workflows/release.yml` — Deleted; all functionality preserved in deploy.yml

### Learnings & Notes

- test-web.yml uses `defaults.run.working-directory: services/web` and `cache-dependency-path` for npm caching, avoiding cd-based patterns
- deploy.yml bump-versions job targets `infra/environments/main.tf` which doesn't exist yet — will be created in flight leg 3 (Terraform consolidation)
- deploy-web uses `cloudflare/wrangler-action@v3` with `workingDirectory` parameter rather than `defaults.run.working-directory` since the action needs it explicitly
- The build+release jobs were copied verbatim from the old release.yml to avoid regressions

### Issues Encountered

- None — implementation straightforward

### Current Status

- Tests: pass (all Go modules green, unchanged)
- Lint: pass (go vet clean)
- Format: pass (prettier reformatting committed separately)

### Next Steps (Flight Leg 3: Terraform Consolidation)

- bossanova-bbc2: Add CF Pages project to cloudflare Terraform module
- bossanova-ldes: Consolidate Terraform envs into single workspace-keyed config
- bossanova-880l: Add *.tfvars to .gitignore
- bossanova-8y9w: [HANDOFF] Review Flight Leg 3

### Resume Command

To continue this work:

1. Run `bd ready --label "flight:fp-2026-03-17-production-readiness-gaps"` — should show bossanova-bbc2
2. Review files: `infra/modules/cloudflare/main.tf`, `infra/modules/cloudflare/variables.tf`, `infra/environments/staging/main.tf`, `infra/environments/production/main.tf`
