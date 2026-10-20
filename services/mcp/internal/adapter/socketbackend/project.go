package socketbackend

import (
	"context"
	"os/exec"
	"strings"

	"github.com/recurser/bossalib/config"
	"github.com/recurser/bossalib/daemonstate"
	"github.com/recurser/bossalib/projectconfig"
)

// appDataDirFor is the app data dir settings resolve to, as bossd and the CLI
// resolve it: the configured one, else the default.
func appDataDirFor(settings config.Settings) (string, error) {
	if dir, ok, err := config.ConfiguredAppDataDir(settings); err != nil {
		return "", err
	} else if ok {
		return dir, nil
	}
	return config.DefaultAppDataDir()
}

// ProjectDaemonIdentity is the daemon this backend dials, by the rule the CLI
// shares (projectconfig.LocalIdentityFromState): the daemon-state record
// bossd wrote under the settings' app data dir, trusted only when it names
// this backend's socket, else the display name the settings resolve to, with
// a warning. A missing or unreadable record degrades, never errors.
func (b *Backend) ProjectDaemonIdentity(context.Context) (projectconfig.DaemonIdentity, string, error) {
	var settings config.Settings
	if p, err := b.settingsPath(); err == nil && p != "" {
		if s, err := config.LoadFrom(p); err == nil {
			settings = s
		}
	}
	hostname, _ := b.hostname()
	fallback := config.DaemonDisplayName(settings, hostname)
	dir, err := b.appDataDir(settings)
	if err != nil {
		id, warning := projectconfig.LocalIdentityFromState(daemonstate.Metadata{}, err, b.socketPath, fallback)
		return id, warning, nil
	}
	md, readErr := daemonstate.Read(dir)
	id, warning := projectconfig.LocalIdentityFromState(md, readErr, b.socketPath, fallback)
	return id, warning, nil
}

// CheckoutBranch is the branch checked out at dir; ok is false for a
// detached HEAD or a directory git cannot read.
func (b *Backend) CheckoutBranch(ctx context.Context, dir string) (string, bool) {
	out, err := exec.CommandContext(ctx, "git", "-C", dir, "symbolic-ref", "--short", "-q", "HEAD").Output()
	branch := strings.TrimSpace(string(out))
	if err != nil || branch == "" {
		return "", false
	}
	return branch, true
}
