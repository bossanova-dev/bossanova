# Plan: Production Readiness Gaps

## Context

All 12 flight legs from the Go rewrite plan are already implemented (24 sub-legs). Both Leg 8 (Auth + Terraform) and Leg 11 (Web SPA) are complete. What remains are **operational gaps** needed to actually deploy and run in production.

### Domain Scheme

Cloudflare Universal SSL only covers `*.bossanova.dev` (one level), so staging uses hyphens not sub-sub-domains. Auth0 uses a single tenant (`id.bossanova.dev`) shared across all envs, with separate Auth0 applications per env.

| Service                | Staging                                 | Production                        |
| ---------------------- | --------------------------------------- | --------------------------------- |
| Orchestrator (Fly.io)  | `orchestrator-staging.bossanova.dev`    | `orchestrator.bossanova.dev`      |
| Auth0 (shared tenant)  | `id.bossanova.dev`                      | `id.bossanova.dev`                |
| Web SPA (CF Pages)     | `app-staging.bossanova.dev`             | `app.bossanova.dev`               |

## Changes

### 1. Fix `DefaultDBPath()` to honor `BOSSO_DB_PATH`

**File:** `services/bosso/internal/db/db.go`

Currently hardcodes `~/Library/Application Support/bossanova/bosso.db` (macOS-only). The Terraform module already sets `BOSSO_DB_PATH=/data/bosso.db` but the code ignores it. Without this fix, containerized bosso fails.

```go
func DefaultDBPath() (string, error) {
    if p := os.Getenv("BOSSO_DB_PATH"); p != "" {
        if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
            return "", fmt.Errorf("create data dir: %w", err)
        }
        return p, nil
    }
    // existing macOS fallback unchanged...
}
```

### 2. Dockerfile for bosso

**File:** `services/bosso/Dockerfile` (built from repo root context)

Multi-stage:

1. **Builder** (`golang:1.25-bookworm`): Copy `go.work`, `go.work.sum`, module `go.mod`/`go.sum` files. Create stub `go.mod` for boss/bossd (referenced by go.work but not needed). `go mod download` for caching. Copy bossalib + bosso source. Build with `CGO_ENABLED=0`.
2. **Litestream** (`litestream/litestream:0.3`): Pull binary.
3. **Final** (`debian:bookworm-slim`): ca-certificates + bosso + litestream. Entrypoint: `litestream replicate -exec bosso`.

### 3. Litestream config

**File:** `services/bosso/litestream.yml`

Replicates `/data/bosso.db` to R2 via env vars (`LITESTREAM_BUCKET`, `LITESTREAM_ENDPOINT`, `LITESTREAM_ACCESS_KEY_ID`, `LITESTREAM_SECRET_ACCESS_KEY`). `force-path-style: true` for R2. `sync-interval: 10s`.

### 4. Fly.io config

**File:** `services/bosso/fly.toml`

Fly v2 format: `[http_service]` on port 8080, `[mounts]` for `/data` volume, `auto_stop_machines = false` (SQLite = single instance), health check.

### 5. Deploy workflow (tag-triggered)

**File:** `.github/workflows/deploy.yml` — replaces existing `release.yml`

Triggered on `v*` tag push. Three parallel deployment jobs:

**Job: deploy-bosso**

- `docker/setup-buildx-action@v3` + `docker/build-push-action@v5`
- Push to `registry.fly.io/bosso:$TAG`
- `superfly/flyctl-actions/setup-flyctl` + `fly deploy --image ...`
- Secrets: `FLY_API_TOKEN`
- GHA cache for Docker layers

**Job: deploy-web** (madverts CF Pages pattern)

- `actions/setup-node@v4`, `npm ci`, `npm run build`
- `cloudflare/wrangler-action@v3` with `pages deploy dist --project-name=bossanova-web --branch=main`
- Secrets: `CLOUDFLARE_API_TOKEN`

**Job: release** (existing CLI binary release, moved from release.yml)

- Cross-platform boss/bossd builds
- `softprops/action-gh-release@v2`

**Job: bump-versions** (runs after deploy-bosso succeeds, madverts pattern)

- `sed` updates `bosso_image` in `infra/environments/main.tf` with new tag
- Commits and pushes `[skip ci]` to main

### 6. Web SPA CI workflow

**File:** `.github/workflows/test-web.yml`

Same dorny/paths-filter two-job pattern as Go services. Steps: `npm ci`, `npm run build`, `npm run lint`. Triggers on `services/web/**` and `proto/**`.

### 7. Terraform: single config with workspaces + env maps

Consolidate `infra/environments/staging/` and `infra/environments/production/` into a single `infra/environments/` directory. Use terraform workspaces (`staging`, `production`) with `terraform.workspace` as the key into env-specific maps. Non-sensitive config lives in `locals` maps; only sensitive values remain as `variable` inputs.

**Delete:** `infra/environments/staging/`, `infra/environments/production/`

**File:** `infra/environments/main.tf` — single config with workspace-keyed locals:

```hcl
locals {
  env    = terraform.workspace
  domain = "bossanova.dev"

  # Hyphenated suffix for non-production: "orchestrator-staging" vs "orchestrator"
  suffix = local.env == "production" ? "" : "-${local.env}"

  # Non-sensitive infrastructure IDs (safe to commit)
  fly_org              = "recurser"
  cloudflare_account_id = "your-account-id"  # TODO: fill in real value
  cloudflare_zone_id    = "your-zone-id"     # TODO: fill in real value

  config = {
    staging = {
      fly_app_name       = "bosso-staging"
      fly_region         = "sjc"
      fly_cpus           = 1
      fly_memory_mb      = 256
      fly_volume_size_gb = 1
      bosso_image        = "registry.fly.io/bosso-staging:v0.1.0" # updated by deploy workflow
      pages_project_name = "bossanova-web-staging"
      auth0_client_name  = "Bossanova Staging"
      r2_bucket_name     = "bosso-litestream-staging"
    }
    production = {
      fly_app_name       = "bosso"
      fly_region         = "sjc"
      fly_cpus           = 2
      fly_memory_mb      = 512
      fly_volume_size_gb = 2
      bosso_image        = "registry.fly.io/bosso:v0.1.0" # updated by deploy workflow
      pages_project_name = "bossanova-web"
      auth0_client_name  = "Bossanova"
      r2_bucket_name     = "bosso-litestream"
    }
  }

  c = local.config[local.env]

  # Derived domains (hyphenated for CF Universal SSL compatibility)
  api_subdomain = "orchestrator${local.suffix}"              # orchestrator | orchestrator-staging
  api_domain    = "${local.api_subdomain}.${local.domain}"   # orchestrator.bossanova.dev
  app_domain    = "app${local.suffix}.${local.domain}"       # app.bossanova.dev | app-staging.bossanova.dev
  api_audience  = "https://${local.api_domain}"

  # Auth0: single tenant shared across all envs, separate apps per env
  auth0_issuer  = "https://id.${local.domain}/"

  cors_origins  = ["https://${local.app_domain}"]
  web_origins   = ["https://${local.app_domain}"]
  callback_urls = ["http://127.0.0.1/callback"]
  logout_urls   = ["https://${local.app_domain}"]
}

module "fly" {
  source         = "../modules/fly"
  app_name       = local.c.fly_app_name
  org            = local.fly_org
  region         = local.c.fly_region
  image          = local.c.bosso_image
  cpus           = local.c.fly_cpus
  memory_mb      = local.c.fly_memory_mb
  volume_size_gb = local.c.fly_volume_size_gb
  oidc_issuer    = local.auth0_issuer      # same issuer for all envs
  oidc_audience  = local.api_audience      # env-specific audience
}

module "auth0" {
  source        = "../modules/auth0"
  api_audience  = local.api_audience
  client_name   = local.c.auth0_client_name
  callback_urls = local.callback_urls
  web_origins   = local.web_origins
  logout_urls   = local.logout_urls
}

module "cloudflare" {
  source              = "../modules/cloudflare"
  account_id          = local.cloudflare_account_id
  zone_id             = local.cloudflare_zone_id
  domain              = local.domain
  api_subdomain       = local.api_subdomain
  fly_hostname        = module.fly.app_hostname
  r2_bucket_name      = local.c.r2_bucket_name
  pages_project_name  = local.c.pages_project_name
  pages_custom_domain = local.app_domain
}

module "github" {
  source          = "../modules/github"
  installation_id = var.github_app_installation_id
  webhook_url     = "https://${local.api_domain}/webhooks/github"
}
```

**File:** `infra/environments/variables.tf` — only truly sensitive secrets:

```hcl
# No variables needed — all non-sensitive config is in locals.
# Sensitive secrets (social login) are passed via TF_VAR_* env vars if needed.
variable "google_client_secret" { type = string; default = ""; sensitive = true }
variable "github_client_secret" { type = string; default = ""; sensitive = true }
```

**Usage:**

```bash
cd infra/environments
terraform workspace select staging   # or: terraform workspace new staging
terraform plan
terraform workspace select production
terraform plan
```

**Gitops version bump:** The deploy workflow commits a `sed` update to `bosso_image` in `infra/environments/main.tf` after a successful deploy (same pattern as madverts `bump-versions` job). This means `terraform apply` always uses the last successfully-deployed image tag.

### 8. Terraform: add CF Pages project to cloudflare module

**File:** `infra/modules/cloudflare/main.tf` — add:

```hcl
resource "cloudflare_pages_project" "web" {
  account_id        = var.account_id
  name              = var.pages_project_name
  production_branch = "main"
}

resource "cloudflare_pages_domain" "web" {
  account_id   = var.account_id
  project_name = cloudflare_pages_project.web.name
  domain       = var.pages_custom_domain
}
```

**File:** `infra/modules/cloudflare/variables.tf` — add `pages_project_name`, `pages_custom_domain`.

### 9. .gitignore for tfvars

Add `*.tfvars` and `!*.tfvars.example` to `.gitignore` (safety net — currently no tfvars files needed since everything is in locals, but protects future additions).

## Execution Order

1. Fix `db.go` DefaultDBPath
2. Create `litestream.yml`
3. Create `Dockerfile`
4. Create `fly.toml`
5. Create `.github/workflows/test-web.yml`
6. Create `.github/workflows/deploy.yml` (consolidates release.yml + adds bosso deploy + web deploy + version bump)
7. Delete `.github/workflows/release.yml`
8. Add CF Pages project to cloudflare module
9. Consolidate terraform: delete `staging/` and `production/` subdirs, create single `infra/environments/main.tf` + `variables.tf` with workspace-keyed locals (all non-sensitive config in locals maps)
10. Add `*.tfvars` to .gitignore

## Critical Files

| File                                        | Action                              |
| ------------------------------------------- | ----------------------------------- |
| `services/bosso/internal/db/db.go`          | Modify                              |
| `services/bosso/Dockerfile`                 | Create                              |
| `services/bosso/litestream.yml`             | Create                              |
| `services/bosso/fly.toml`                   | Create                              |
| `.github/workflows/deploy.yml`              | Create                              |
| `.github/workflows/release.yml`             | Delete (merged into deploy.yml)     |
| `.github/workflows/test-web.yml`            | Create                              |
| `infra/modules/cloudflare/main.tf`          | Modify (add pages_project)          |
| `infra/modules/cloudflare/variables.tf`     | Modify (add pages vars)             |
| `infra/environments/staging/`               | Delete (merged into single config)  |
| `infra/environments/production/`            | Delete (merged into single config)  |
| `infra/environments/main.tf`                | Create (workspace-keyed locals)     |
| `infra/environments/variables.tf`           | Create (secrets only)               |
| `.gitignore`                                | Modify (add \*.tfvars)              |

## Verification

- `docker build -f services/bosso/Dockerfile -t bosso:test .` from repo root
- `make test` passes (db.go change is backward-compatible)
- Push to non-main branch: `test-web.yml` triggers and passes
- `terraform plan` in staging workspace shows valid plan
- Tag push: `deploy.yml` triggers with bosso + web + release + version-bump jobs
