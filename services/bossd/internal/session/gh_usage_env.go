package session

import (
	"maps"

	"github.com/recurser/bossalib/models"

	"github.com/recurser/bossd/internal/ghusage"
)

// ManagedSessionEnv returns BOSS_* context, omitting unresolved binaries, plus
// the gh wrapper variables that meter the chat's gh calls (none when gh is nil).
// Unattended markers match BuildAppendSystemPrompt; cron identity is cron-only.
//
// agentName is the running agent for this chat. It takes precedence over
// sess.AgentName, so a codex chat spawned from a claude session correctly
// reports BOSS_AGENT=codex. Pass "" to fall back to sess.AgentName.
func ManagedSessionEnv(sess *models.Session, agentSessionID, agentName string, gh *ghusage.Shim) map[string]string {
	return meteredSessionEnv(ResolveSessionFacts(sess, agentSessionID, agentName), gh)
}

// meteredSessionEnv is managedSessionEnv with the gh wrapper first on PATH.
// The managed env is daemon-supplied and wins over the worktree .env, so a
// repo cannot take the wrapper off the spawned PATH. A login shell can still
// put the real gh first (macOS path_helper, brew shellenv): Claude Code puts
// the wrapper back before each Bash command via CLAUDE_ENV_FILE, but another
// agent's login shell keeps that order, so its calls go unmetered, never failed.
func meteredSessionEnv(f Facts, gh *ghusage.Shim) map[string]string {
	env := managedSessionEnv(f)
	maps.Copy(env, gh.Env(ghUsageLabel(f)))
	return env
}

// ghUsageLabel attributes a managed chat's gh calls in the usage ledger: a
// cron run by its cron name, every other chat by the kind of session it is.
func ghUsageLabel(f Facts) string {
	switch {
	case f.IsCron:
		return ghusage.LabelPrefixCron + f.CronName
	case f.IsQuickChat:
		return ghusage.LabelPrefixSession + "quick-chat"
	case f.IsUnattended:
		return ghusage.LabelPrefixSession + "unattended"
	default:
		return ghusage.LabelPrefixSession + "interactive"
	}
}

// SetGHUsageShim installs the daemon's gh wrapper, which every managed chat
// spawn puts first on PATH. Nil (the default) leaves chats unmetered.
func (l *Lifecycle) SetGHUsageShim(gh *ghusage.Shim) { l.ghUsageShim = gh }
