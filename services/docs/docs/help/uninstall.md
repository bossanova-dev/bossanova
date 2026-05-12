---
title: Uninstall
description: How to fully remove Bossanova binaries, the daemon launch agent, and on-disk data.
---

# Uninstall

```bash
# Stop and remove daemon (uninstalls the LaunchAgent so it stops and
# does not restart)
boss daemon uninstall
launchctl bootout gui/$(id -u) ~/Library/LaunchAgents/com.bossanova.bossd.plist 2>/dev/null || true
rm -f ~/Library/LaunchAgents/com.bossanova.bossd.plist

# Remove binaries
brew uninstall bossanova-dev/tap/bossanova

# Remove data (optional; includes worktrees under ~/.bossanova/worktrees)
rm -rf ~/.bossanova
```
