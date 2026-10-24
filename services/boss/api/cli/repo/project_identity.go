package repo

import (
	"github.com/recurser/bossalib/daemonstate"
	"github.com/recurser/bossalib/projectconfig"
)

// localApplyIdentity reads this daemon's routing id and display name from the
// daemon-state record bossd writes at startup, trusted only when it names this
// CLI's socket (projectconfig.LocalIdentityFromState, the rule the local MCP
// server shares). When the record is missing, has no id or names another
// socket, matching degrades to the display name the settings resolve to, and
// the warning says so: entries pinned by daemonId then never match, so this
// machine's pinned crons are not applied until bossd restarts.
func localApplyIdentity(env projectExportEnv) (projectconfig.DaemonIdentity, string) {
	profile, err := env.profile()
	if err != nil {
		return projectconfig.LocalIdentityFromState(daemonstate.Metadata{}, err, "", fallbackDaemonName(env, ""))
	}
	md, readErr := daemonstate.Read(profile.AppDataDir)
	return projectconfig.LocalIdentityFromState(md, readErr, profile.SocketPath, fallbackDaemonName(env, profile.SettingsPath))
}
