---
title: Installation
description: Install Bossanova via Homebrew, curl, or build from source.
---

# Installation

## Via Homebrew (recommended)

```bash
brew install bossanova-dev/tap/bossanova
```

No separate `brew tap` step is required. The fully qualified formula
name points Homebrew at the Bossanova tap during install.

## Prerequisites

- A coding-agent CLI matching the agent runner plugin you intend to
  use. The bundled `claude` plugin requires the
  [Claude Code CLI](https://claude.ai/download); the bundled `codex`
  plugin requires the
  [OpenAI Codex CLI](https://help.openai.com/en/articles/11096431-openai-codex-cli-getting-started).
  `opencode` remains on the roadmap.
- [GitHub CLI](https://cli.github.com/): required for PR operations.

## Manual installation via curl

```bash
curl -fsSL https://bossanova.dev/install.sh | sh
```

The curl installer downloads the latest GitHub Release binaries for
macOS (`darwin-amd64`, `darwin-arm64`) or Linux (`linux-amd64`). It
checks for a supported coding-agent CLI (`claude` or `codex`), GitHub
CLI, and a SHA-256 tool before installing.

## Build from source

Requires macOS with [Homebrew](https://brew.sh/). The `make deps` target
installs everything else (`go`, `buf`, `golangci-lint`, `jq`, `gh`,
`gremlins`, and the `protoc-gen-go` / `protoc-gen-connect-go` buf
plugins).

```bash
git clone https://github.com/bossanova-dev/bossanova.git
cd bossanova
make deps
make
```

Binaries land in `bin/`. The Go-based buf plugins install into
`$(go env GOPATH)/bin` (usually `~/go/bin`). If that directory isn't on
your `PATH`, `make deps` will print the command to add it.

### Useful targets

| Target         | What it does                                                     |
| -------------- | ---------------------------------------------------------------- |
| `make build`   | Build `boss` and `bossd` only (skips plugins and cross-compiles) |
| `make plugins` | Build the `bossd-plugin-*` binaries                              |
| `make test`    | Run tests across all modules                                     |
| `make lint`    | Run `golangci-lint` and `buf lint`                               |
| `make clean`   | Remove `bin/` and generated code                                 |

## Verify your install

Run `boss repair doctor`. It checks that the daemon can find a working
agent runner plugin and reports any failures it sees.

If the `agent runner client wired` check fails, confirm that at least
one runner binary (`bossd-plugin-claude` or `bossd-plugin-codex`) sits
next to `bossd` or in the Homebrew plugin directory. Then make sure the
matching CLI (`claude` or `codex`) is on `bossd`'s `PATH`. Re-run
`boss repair doctor` and confirm all checks pass before launching
`boss`.
