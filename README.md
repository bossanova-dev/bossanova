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
- **bossd-plugin-autopilot** - Autonomous PR creation and merging
- **bossd-plugin-dependabot** - Automatic dependency update PR handling
- **bossd-plugin-repair** - Automated PR conflict resolution and CI fixes

## Prerequisites

- [Claude Code CLI](https://claude.ai/download) - Required for session management
- [GitHub CLI](https://cli.github.com/) - Required for PR operations

## How It Works

Bossanova uses git worktrees to isolate each Claude Code session in its own directory. The daemon (bossd) manages session lifecycle, monitors PR status, and coordinates plugins. The TUI (boss) provides a unified view across all active sessions.

Sessions run in dedicated worktrees, allowing simultaneous work on multiple features without conflicts. Plugins listen for events (PR creation, CI failures, merge conflicts) and take autonomous actions.

## Alternative Install

**Note**: Manual installation via curl is not yet supported. Use Homebrew for now:

```bash
brew install bossanova-dev/tap/bossanova
```

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
