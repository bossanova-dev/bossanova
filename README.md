# bossanova

Manage multiple Claude Code sessions from one terminal.

![Bossanova TUI showing 6 Claude Code sessions with status indicators across multiple repos](docs/screenshot.png)

## Install

```bash
brew install bossanova-dev/tap/bossanova
```

## Quick Start

1. Install bossanova
2. Add a repository: `boss repo add /path/to/your/repo`
3. Launch the TUI: `boss`

## What You Get

- **boss** - Terminal UI for managing Claude Code sessions across repositories
- **bossd** - Background daemon handling session lifecycle and git operations

Bossd supports plugins (`bossd-plugin-*` binaries) for autonomous PR handling,
dependency updates, CI repair, and other automation.

## Prerequisites

- [Claude Code CLI](https://claude.ai/download) - Required for session management
- [GitHub CLI](https://cli.github.com/) - Required for PR operations

## How It Works

Bossanova uses git worktrees to isolate each Claude Code session in its own directory. The daemon (bossd) manages session lifecycle, monitors PR status, and coordinates plugins. The TUI (boss) provides a unified view across all active sessions.

Sessions run in dedicated worktrees, allowing simultaneous work on multiple features without conflicts. Plugins listen for events (PR creation, CI failures, merge conflicts) and take autonomous actions.

## Setup Scripts

Each repository can have an optional setup script that runs automatically whenever a new worktree is created for a session. This is useful for installing dependencies, copying configuration files, or any other per-worktree initialization.

### Configuring

Set a setup script when adding a repo, or update it later:

```bash
boss repo update my-repo --setup-script "npm install"
```

Clear it with an empty string:

```bash
boss repo update my-repo --setup-script ""
```

### Environment Variables

The following environment variables are available to the setup script:

| Variable | Description |
|---|---|
| `BOSS_REPO_DIR` | Path to the main git repository (the original clone) |
| `BOSS_WORKTREE_DIR` | Path to the worktree being set up |

These let you reference files in the main repo without hardcoding paths. For example, to copy an `.env` file into each new worktree:

```bash
boss repo update my-repo --setup-script 'cp "$BOSS_REPO_DIR/.env" "$BOSS_WORKTREE_DIR/.env" && npm install'
```

## Alternative Install

**Note**: Manual installation via curl is not yet supported. Use Homebrew for now:

```bash
brew install bossanova-dev/tap/bossanova
```

## Build from Source

Requires macOS with [Homebrew](https://brew.sh/). The `make deps` target
installs everything else (`go`, `buf`, `golangci-lint`, `jq`, `gh`,
`gremlins`, and the `protoc-gen-go`/`protoc-gen-connect-go` buf plugins).

```bash
git clone https://github.com/recurser/bossanova.git
cd bossanova
make deps
make
```

Binaries land in `bin/`. The Go-based buf plugins install into `$(go env GOPATH)/bin`
(usually `~/go/bin`) — if that directory isn't on your `PATH`, `make deps` will
print the command to add it.

Other useful targets:

| Target | What it does |
|---|---|
| `make build` | Build `boss` and `bossd` only (skips plugins and cross-compiles) |
| `make plugins` | Build the `bossd-plugin-*` binaries |
| `make test` | Run tests across all modules |
| `make lint` | Run `golangci-lint` and `buf lint` |
| `make clean` | Remove `bin/` and generated code |

## Uninstall

```bash
# Stop and remove daemon
boss daemon stop
launchctl bootout gui/$(id -u) ~/Library/LaunchAgents/com.bossanova.bossd.plist
rm ~/Library/LaunchAgents/com.bossanova.bossd.plist

# Remove binaries
brew uninstall boss

# Or if installed via curl|sh:
rm /usr/local/bin/boss*
rm /usr/local/bin/bossd*

# Remove data (optional)
rm -rf ~/.boss
```
