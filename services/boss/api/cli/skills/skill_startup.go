package skills

import (
	"fmt"
	"os"
	"strings"

	"github.com/recurser/boss/internal/termreset"
	libskillinstall "github.com/recurser/bossalib/contract/plugin/skillinstall"
	"github.com/recurser/bossalib/kit/config"
)

type skillInstallAgent struct {
	name    string
	command string
	agent   libskillinstall.Agent
}

// maybeInstallSkills prompts the user to install or update boss skills for each
// available coding agent on startup. Prompts are shown in agent order.
func (g *Group) maybeInstallSkills() error {
	if os.Getenv("BOSS_SKIP_SKILLS") != "" {
		return nil
	}
	if !g.StartupIsTerminal() {
		// Headless (daemon/tmux/cron) boss invocations previously bailed here,
		// so a merged skill edit never reached live sessions. Instead of
		// prompting, silently self-heal a stale-but-installed tree (update-only,
		// never a fresh install into an empty dir) so the on-disk global skills
		// track the checkout payload when available, otherwise this binary's
		// embed.
		return g.selfHealSkills()
	}
	payload, err := g.startupSkillPayload()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Warning: skipping checkout skill refresh: %v\n", err)
		return nil // non-fatal
	}
	manifest, err := libskillinstall.Manifest(payload.fsys)
	if err != nil {
		return nil // non-fatal
	}
	settings, loadErr := config.Load()
	settingsChanged := false
	// An accepted prompt is operator consent, so it writes an explicit record;
	// the same payload judged as an unattended refresh decides only whether a
	// provably older payload is offered at all.
	explicitRecord := g.skillPayloadRecord(payload, libskillinstall.WriterExplicit)
	order := g.skillPayloadOrder()

	for _, target := range g.skillInstallAgents {
		if _, err := g.StartupLookPath(target.command); err != nil {
			continue
		}
		dir, err := libskillinstall.DirForAgent(target.agent)
		if err != nil {
			continue
		}
		installed := libskillinstall.IsInstalled(dir)
		needsUpdate := false
		if installed {
			needsUpdate, err = libskillinstall.NeedsUpdate(dir, payload.fsys)
			if err != nil {
				fmt.Fprintf(os.Stderr, "Warning: failed to check %s skills: %v\n", target.name, err)
				continue
			}
		}
		agentName := string(target.agent)
		if installed && !needsUpdate {
			if rememberInstalledSkillManifest(&settings, agentName, manifest) {
				settingsChanged = true
			}
			continue
		}
		if skillPromptDeclined(settings, target.agent, installed, manifest) {
			continue
		}
		if decision, older := skillPayloadProvablyOlder(dir, installed, explicitRecord, order); older {
			// A held notice is not a decline; a newer binary can still update.
			settingsChanged = reportSkillRefreshHold(&settings, loadErr == nil, target, decision, explicitRecord) || settingsChanged
			continue
		}

		action := "Install"
		preposition := "to"
		if installed {
			action = "Update"
			preposition = "in"
		}
		// Clear any stranded input-reporting modes before blocking on the
		// prompt. This hook runs ahead of the TUI's own self-heal, so without
		// this a user relaunching boss to fix a terminal that is spewing mouse
		// reports would have that garbage echoed into the answer line — and
		// fmt.Scanln would consume it as a non-empty, non-"n" answer. Acute on
		// the first run after an upgrade, which is exactly when a payload change
		// makes this prompt appear. See BOS-650.
		termreset.WriteAbnormalExitResetIfTerminal(os.Stderr)
		fmt.Fprintf(os.Stderr, "%s boss skills for %s %s %s? [Y/n] ", action, target.name, preposition, dir)
		answer := strings.ToLower(strings.TrimSpace(g.StartupReadAnswer()))
		if answer == "n" || answer == "no" {
			if rememberDeclinedSkillPrompt(&settings, agentName, manifest) {
				settingsChanged = true
			}
			continue
		}
		if err := libskillinstall.ExtractRecorded(dir, payload.fsys, explicitRecord); err != nil {
			fmt.Fprintf(os.Stderr, "Warning: failed to install %s skills: %v\n", target.name, err)
			continue
		}
		if recordSkillInstall(&settings, target.agent, manifest) {
			settingsChanged = true
		}
		if installed {
			fmt.Fprintf(os.Stderr, "Boss skills updated for %s.\n", target.name)
		} else {
			fmt.Fprintf(os.Stderr, "Boss skills installed for %s.\n", target.name)
		}
	}
	saveSkillSettings(settings, settingsChanged, loadErr)
	warnBinarySkillsDrift(payload, false, manifest)
	return nil
}

func skillPromptDeclined(settings config.Settings, agent libskillinstall.Agent, installed bool, manifest string) bool {
	agentName := string(agent)
	declined := settings.SkillsDeclinedByAgent[agentName]
	declinedManifest := settings.SkillsDeclinedManifestByAgent[agentName]
	if agent == libskillinstall.AgentClaude && settings.SkillsDeclined && !declined && declinedManifest == "" {
		return !installed
	}
	return declined && declinedManifest == manifest
}

func rememberDeclinedSkillPrompt(settings *config.Settings, agentName, manifest string) bool {
	if settings.SkillsDeclinedByAgent == nil {
		settings.SkillsDeclinedByAgent = map[string]bool{}
	}
	if settings.SkillsDeclinedManifestByAgent == nil {
		settings.SkillsDeclinedManifestByAgent = map[string]string{}
	}
	changed := !settings.SkillsDeclinedByAgent[agentName] || settings.SkillsDeclinedManifestByAgent[agentName] != manifest
	settings.SkillsDeclinedByAgent[agentName] = true
	settings.SkillsDeclinedManifestByAgent[agentName] = manifest
	return changed
}

func rememberInstalledSkillManifest(settings *config.Settings, agentName, manifest string) bool {
	if settings.SkillsInstalledManifestByAgent == nil {
		settings.SkillsInstalledManifestByAgent = map[string]string{}
	}
	changed := settings.SkillsInstalledManifestByAgent[agentName] != manifest
	settings.SkillsInstalledManifestByAgent[agentName] = manifest
	return changed
}

func clearDeclinedSkillPrompt(settings *config.Settings, agentName string) bool {
	changed := false
	if settings.SkillsDeclinedByAgent != nil {
		if settings.SkillsDeclinedByAgent[agentName] {
			changed = true
		}
		delete(settings.SkillsDeclinedByAgent, agentName)
	}
	if settings.SkillsDeclinedManifestByAgent != nil {
		if _, ok := settings.SkillsDeclinedManifestByAgent[agentName]; ok {
			changed = true
		}
		delete(settings.SkillsDeclinedManifestByAgent, agentName)
	}
	return changed
}

// recordSkillInstall applies the post-extract settings bookkeeping shared by the
// interactive prompt, the non-interactive `boss skills` command, and the headless
// self-heal: record the installed manifest, clear any declined-prompt state, and
// clear the legacy Claude decline bit. Returns whether settings changed.
func recordSkillInstall(settings *config.Settings, agent libskillinstall.Agent, manifest string) bool {
	agentName := string(agent)
	changed := rememberInstalledSkillManifest(settings, agentName, manifest)
	if _, ok := settings.SkillsHeldNoticeByAgent[agentName]; ok {
		delete(settings.SkillsHeldNoticeByAgent, agentName)
		changed = true
	}
	if clearDeclinedSkillPrompt(settings, agentName) {
		changed = true
	}
	if agent == libskillinstall.AgentClaude {
		if settings.SkillsDeclined {
			changed = true
		}
		settings.SkillsDeclined = false
	}
	return changed
}

// selfHealSkills refreshes stale-but-installed skill trees for each agent on PATH
// without prompting. It never fresh-installs into an empty target (a headless
// first run should not silently populate a user's global skills dir) and never
// prints a [Y/n] prompt, so daemon/tmux/cron `boss` invocations keep the global
// skills current instead of bailing. The caller has already honored
// BOSS_SKIP_SKILLS and the non-TTY check.
func (g *Group) selfHealSkills() error {
	payload, err := g.startupSkillPayload()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Warning: skipping checkout skill refresh: %v\n", err)
		return nil // non-fatal
	}
	manifest, err := libskillinstall.Manifest(payload.fsys)
	if err != nil {
		return nil // non-fatal
	}
	// A malformed/unreadable settings.json makes config.Load return default
	// settings alongside an error. Saving those defaults would silently replace
	// the user's real file, so on a load error we still refresh the skill trees
	// (the point of self-heal) but skip all manifest bookkeeping and the save.
	settings, loadErr := config.Load()
	if loadErr != nil {
		fmt.Fprintf(os.Stderr, "Warning: skipping skill-manifest bookkeeping; failed to load settings: %v\n", loadErr)
	}
	settingsChanged := false
	refreshed := false
	installedTargetsCurrent := true
	// A trusted checkout is an explicit opt-in, so it keeps today's overwrite
	// and stamps an explicit record. The embedded payload is the unattended
	// case — any peer session calling an older boss — and goes through the
	// no-downgrade guard.
	guarded := !payload.fromSource
	writer := libskillinstall.WriterExplicit
	if guarded {
		writer = libskillinstall.WriterUnattended
	}
	record := g.skillPayloadRecord(payload, writer)
	order := g.skillPayloadOrder()
	for _, target := range g.skillInstallAgents {
		dir, err := libskillinstall.DirForAgent(target.agent)
		if err != nil {
			installedTargetsCurrent = false
			continue
		}
		if _, err := g.StartupLookPath(target.command); err != nil {
			if libskillinstall.IsInstalled(dir) {
				installedTargetsCurrent = false
			}
			continue
		}
		// Honor a previously declined update for this exact manifest. An
		// interactive `n` records the decline via rememberDeclinedSkillPrompt,
		// and the interactive path skips the update through skillPromptDeclined.
		// Silently refreshing here would reduce that opt-out to interactive
		// sessions only — every headless daemon/tmux/cron `boss` invocation
		// would overwrite the declined tree. Explicit `boss skills sync/install`
		// remains the override. On a settings load error we cannot read the
		// decline state, so we fall through and refresh (matching the
		// bookkeeping skip above); default settings carry no declines anyway.
		if loadErr == nil && skillPromptDeclined(settings, target.agent, true, manifest) {
			installedTargetsCurrent = false
			continue
		}
		updated, held, decision, err := selfHealAgentSkills(dir, payload, record, guarded, order)
		if err != nil {
			installedTargetsCurrent = false
			fmt.Fprintf(os.Stderr, "Warning: failed to refresh %s skills: %v\n", target.name, err)
			continue
		}
		if held {
			// A held refresh left the tree as it was: it is neither current
			// nor recorded as installed, so recordSkillInstall is not called.
			installedTargetsCurrent = false
			settingsChanged = reportSkillRefreshHold(&settings, loadErr == nil, target, decision, record) || settingsChanged
			continue
		}
		if !updated {
			continue
		}
		refreshed = true
		if loadErr == nil && recordSkillInstall(&settings, target.agent, manifest) {
			settingsChanged = true
		}
	}
	saveSkillSettings(settings, settingsChanged, loadErr)
	warnBinarySkillsDrift(payload, refreshed && installedTargetsCurrent, manifest)
	return nil
}

// selfHealAgentSkills refreshes one agent's tree and returns any guarded hold
// decision so the caller can report it with settings-aware notice bookkeeping.
func selfHealAgentSkills(dir string, payload selectedSkillPayload, record libskillinstall.PayloadRecord, guarded bool, order libskillinstall.OrderFunc) (updated, held bool, decision libskillinstall.RefreshDecision, err error) {
	if !guarded {
		updated, err = libskillinstall.EnsureUpdatedRecorded(dir, payload.fsys, record)
		return updated, false, decision, err
	}
	result, err := libskillinstall.EnsureUpdatedGuarded(dir, payload.fsys, record, order)
	return result.Updated, result.Held, result.Decision, err
}

// skillPayloadProvablyOlder reports whether the tree at dir was stamped by a
// payload the oracle proves newer than record. Unprovable order is not
// "older": the interactive prompt still offers it, because the operator is
// there to decide.
func skillPayloadProvablyOlder(dir string, installed bool, record libskillinstall.PayloadRecord, order libskillinstall.OrderFunc) (libskillinstall.RefreshDecision, bool) {
	if !installed {
		return libskillinstall.RefreshDecision{}, false
	}
	decision, err := libskillinstall.InstalledRefreshDecision(dir, record, order)
	older := err == nil && decision.Recorded && !decision.Refresh && decision.Ordering == libskillinstall.OrderOlder
	return decision, older
}

// saveSkillSettings never replaces an unreadable settings file with defaults.
func saveSkillSettings(settings config.Settings, changed bool, loadErr error) {
	if changed && loadErr == nil {
		_ = config.Save(settings)
	}
}
