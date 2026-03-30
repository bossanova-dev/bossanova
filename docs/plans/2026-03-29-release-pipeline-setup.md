# Release Pipeline Setup Guide

How to get the release and distribution infrastructure running after merging PR #61.

---

## Architecture Overview

```
Push to main ──────────────► Mirror workflow
                              │ strips private code
                              ▼
                            bossanova-dev/bossanova (public)
                              │ main branch (stripped)
                              │ production branch (stripped)

Push to production ────────► Release workflow
                              │
                              ├─ semantic-release (version bump)
                              ├─ cross-compile (darwin-amd64, darwin-arm64, linux-amd64)
                              ├─ macOS notarize + sign (optional)
                              ├─ GitHub Release on public repo (binaries + checksums)
                              ├─ Homebrew tap update
                              └─ .VERSION bump on production
```

**Two independent workflows, same trigger branch:**

| Workflow                         | Trigger                        | What it does                                     |
| -------------------------------- | ------------------------------ | ------------------------------------------------ |
| `mirror-public.yml`              | Push to `main` or `production` | Strips private code, force-pushes to public repo |
| `perform-production-release.yml` | Push to `production`           | Semantic version, build, sign, release, Homebrew |

They share a concurrency group (`production-${{ github.ref_name }}`), so they run sequentially when both trigger on a `production` push. Mirror runs first (wins the race), then release tags the already-mirrored commit.

---

## Prerequisites

### External accounts

- **GitHub organization**: `bossanova-dev` (or wherever the public repos live)
- **Apple Developer Program** membership (for macOS notarization — optional for first release)

### Tools (for local testing only)

- `gh` CLI authenticated with access to both `recurser/bossanova` and the public org
- `semantic-release` (npm) if you want to dry-run locally

---

## Step-by-step Setup

### Step 1: Create the public repos

#### 1a. Public mirror repo

Create `bossanova-dev/bossanova`:

- **Visibility**: Public
- **Description**: "AI-powered PR automation for GitHub"
- **No template, no README** — the mirror workflow will populate it
- Initialize empty (no default branch yet — that's fine, the first mirror push creates it)

#### 1b. Homebrew tap repo

Create `bossanova-dev/homebrew-tap`:

- **Visibility**: Public (required — Homebrew taps must be public)
- **Initialize with a README** so the default branch exists
- Create the formula directory:

```bash
gh repo clone bossanova-dev/homebrew-tap
cd homebrew-tap
mkdir -p Formula
touch Formula/.gitkeep
git add Formula/.gitkeep
git commit -m "chore: initialize Formula directory"
git push
```

### Step 2: Create the Personal Access Token

Create a **fine-grained PAT** scoped to the `bossanova-dev` organization:

1. Go to https://github.com/settings/tokens?type=beta
2. **Token name**: `bossanova-release-pipeline`
3. **Expiration**: 90 days (set a calendar reminder to rotate)
4. **Resource owner**: `bossanova-dev`
5. **Repository access**: Select `bossanova-dev/bossanova` and `bossanova-dev/homebrew-tap`
6. **Permissions**:
   - **Contents**: Read and Write (push branches, tags, commits; create releases)
   - **Metadata**: Read (required by default)
7. Generate and copy the token

### Step 3: Add GitHub secrets

Go to https://github.com/recurser/bossanova/settings/secrets/actions and add:

#### Required (minimum viable release)

| Secret                        | Value                            |
| ----------------------------- | -------------------------------- |
| `BOSSANOVA_PUBLIC_DEPLOY_KEY` | The fine-grained PAT from Step 2 |

#### Optional (macOS code signing and notarization)

These are needed for signed/notarized macOS binaries. Without them, the `notarize` job fails but the `release` job falls back to unsigned binaries with a warning.

| Secret                                 | Value                                                                      | How to get it                                                                                        |
| -------------------------------------- | -------------------------------------------------------------------------- | ---------------------------------------------------------------------------------------------------- |
| `APPLE_DEVELOPER_CERTIFICATE_P12`      | Base64-encoded Developer ID Application certificate                        | Export from Keychain Access as .p12, then `base64 -i cert.p12`                                       |
| `APPLE_DEVELOPER_CERTIFICATE_PASSWORD` | Password you set when exporting the .p12                                   | You chose this during export                                                                         |
| `APPLE_SIGNING_IDENTITY`               | Full certificate name, e.g. `Developer ID Application: Your Name (TEAMID)` | `security find-identity -v -p codesigning`                                                           |
| `APPLE_ID`                             | Your Apple ID email                                                        | The email you sign in to developer.apple.com with                                                    |
| `APPLE_APP_SPECIFIC_PASSWORD`          | App-specific password                                                      | Generate at https://appleid.apple.com/account/manage → Sign-In and Security → App-Specific Passwords |
| `APPLE_TEAM_ID`                        | 10-character team ID                                                       | https://developer.apple.com/account → Membership Details                                             |

### Step 4: Create the `production` branch

The release pipeline only triggers on pushes to `production`. This branch represents "what's released" — you promote code to it when you're ready to cut a release.

```bash
git checkout main
git pull origin main
git push origin main:production
```

This initial push triggers both workflows:

1. **Mirror**: strips private code and pushes to `bossanova-dev/bossanova` (both `main` and `production` branches)
2. **Release**: semantic-release analyzes commits on `production` — on the very first run with conventional commits, it will determine the initial version

### Step 5: Verify the first release

After pushing to `production`, watch the Actions tab:

```bash
# Check workflow runs
gh run list --branch production --limit 5

# Watch the release workflow specifically
gh run list --workflow "Perform Production Release" --limit 3
```

**Expected sequence:**

1. Mirror workflow runs (~30s) — check that `bossanova-dev/bossanova` now has code
2. Version job runs — semantic-release decides the version based on commit history
3. Build job runs (3 parallel matrix builds) — cross-compiles all binaries
4. Notarize job runs (if Apple secrets configured) — signs and notarizes macOS binaries
5. Release job runs — creates GitHub Release with binaries on the public repo
6. Homebrew job runs — pushes updated formula to the tap
7. Bump-versions job runs — writes `.VERSION` file to production branch

**Verify end state:**

```bash
# Public repo has code
gh api repos/bossanova-dev/bossanova --jq '.default_branch'

# Release exists with binaries
gh release list --repo bossanova-dev/bossanova

# Homebrew formula was updated
gh api repos/bossanova-dev/homebrew-tap/contents/Formula/bossanova.rb --jq '.name'

# Homebrew install works
brew tap bossanova-dev/tap
brew install bossanova-dev/tap/bossanova
boss version
```

---

## Ongoing Release Process

After initial setup, cutting a release is:

```bash
git push origin main:production
```

Semantic-release reads the conventional commits since the last release tag and decides:

| Commit type                                        | Version bump  | Example                                     |
| -------------------------------------------------- | ------------- | ------------------------------------------- |
| `feat(...)`                                        | Minor (0.X.0) | `feat(boss): add session filtering`         |
| `fix(...)`                                         | Patch (0.0.X) | `fix(daemon): handle socket timeout`        |
| `perf(...)`                                        | Patch         | `perf(bossd): reduce query count`           |
| `BREAKING CHANGE:` in body                         | Major (X.0.0) | Any commit with breaking change footer      |
| `docs`, `style`, `refactor`, `test`, `ci`, `chore` | No release    | `docs(readme): update install instructions` |

If no release-triggering commits exist since the last tag, semantic-release skips and nothing happens.

### Promoting specific commits

To release only up to a specific commit (not all of main):

```bash
git push origin <commit-sha>:production
```

---

## Installation Channels

Once a release exists, users can install via:

### Homebrew (macOS and Linux)

```bash
brew install bossanova-dev/tap/bossanova
```

### Curl installer

```bash
curl -fsSL https://raw.githubusercontent.com/bossanova-dev/bossanova/main/infra/install.sh | sh
```

Downloads boss, bossd, and all plugins from the latest GitHub Release. Verifies SHA256 checksums. Installs to `~/.local/bin` or `/usr/local/bin`. Registers the daemon with launchd (macOS) or systemd (Linux).

### Manual download

Binaries and checksums are attached to each GitHub Release at:
`https://github.com/bossanova-dev/bossanova/releases`

---

## Troubleshooting

### Mirror workflow fails with 403

The PAT doesn't have write access to `bossanova-dev/bossanova`. Check:

- Token is scoped to the correct organization
- Token has Contents: Read & Write
- Token hasn't expired

### Semantic-release finds no commits

On the first run, semantic-release needs at least one `feat:` or `fix:` commit in the history on `production`. If all commits are `chore:` or `docs:`, no version is bumped and the pipeline stops after the version job. Solution: ensure at least one conventional commit with a release type exists.

### Notarize job fails

This is expected if Apple secrets aren't configured. The release job has a fallback:

```yaml
if [ -d artifacts-all/binaries-darwin-arm64-signed ]; then
cp artifacts-all/binaries-darwin-arm64-signed/* artifacts/
else
echo "::warning::Signed darwin-arm64 binaries not found, using unsigned binaries"
cp artifacts-all/binaries-darwin-arm64/* artifacts/
fi
```

Unsigned macOS binaries trigger Gatekeeper warnings (`"bossanova" can't be opened because Apple cannot check it for malicious software`). Users can bypass with `xattr -d com.apple.quarantine boss` but this is not a good experience — configure notarization before distributing widely.

### Homebrew install fails with "formula not found"

1. Check `bossanova-dev/homebrew-tap` is public
2. Check `Formula/bossanova.rb` exists in the tap repo
3. Try `brew tap --force bossanova-dev/tap` to re-clone

### Release creates v1.0.0 unexpectedly

Semantic-release starts at v1.0.0 by default when there are no prior tags. If you want to start at a lower version (e.g., v0.1.0), manually create the initial tag before the first release:

```bash
# On the production branch, before pushing
git tag v0.0.0
git push origin v0.0.0
```

Then the first `feat:` commit will bump to v0.1.0 instead of v1.0.0.

---

## Security Notes

- The PAT has write access to public repos — rotate it regularly
- The mirror workflow uses `fetch-depth: 1` (shallow clone) so git history from the private repo is not pushed to the public repo
- Release tags are placed on the **public repo's stripped commit**, not the private repo's commit, to avoid leaking private code through the tag's commit tree
- The `.releaserc.yml` and `.github/workflows/` are stripped from the public repo by the mirror
- The `[skip ci]` marker on the version bump commit prevents infinite workflow loops
