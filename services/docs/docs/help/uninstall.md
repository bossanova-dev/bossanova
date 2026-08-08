---
title: Uninstall
description: How to fully remove Bossanova binaries, the daemon launch agent, and on-disk data.
---

import CommandTabs from '@site/src/components/CommandTabs';

# Uninstall

Stop and remove the daemon. `boss daemon uninstall` removes the LaunchAgent so
the daemon stops and does not restart:

<CommandTabs
cli="boss daemon uninstall"
/>

Tearing down the launch agent acts on this machine's service manager, so there
is no MCP tool for it.

Then remove the binaries and, optionally, the data. `boss daemon uninstall`
already deleted the plist, so the `launchctl` lines below are a no-op safety net
for an agent that was registered by hand:

```bash
launchctl bootout gui/$(id -u) ~/Library/LaunchAgents/com.bossanova.bossd.plist 2>/dev/null || true
rm -f ~/Library/LaunchAgents/com.bossanova.bossd.plist

# Remove binaries
brew uninstall bossanova-dev/tap/bossanova

# Remove data (optional; includes worktrees under ~/.bossanova/worktrees)
rm -rf ~/.bossanova
```
