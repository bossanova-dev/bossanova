package session

import (
	"encoding/json"
	"os"
	"path/filepath"

	bossanovav1 "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/models"

	"github.com/recurser/bossd/internal/agent"
)

// agentNameCodex is the plugin name of the Codex agent runner, as persisted on
// a session and keyed in the dispatcher's runner registry.
const agentNameCodex = "codex"

// headlessCapabilityProfileFor selects the profile from the resolved agent and
// tracker adapter, after checking the launch's interactivity signals. GitHub's
// command transport validates its tracker through skill preflight rather than MCP.
// Agent names must already be resolved through the dispatcher.
func headlessCapabilityProfileFor(resolvedAgentName, trackerAdapter string, opts StartSessionOpts) bossanovav1.HeadlessCapabilityProfile {
	unattended := opts.Detach || opts.IsTmuxUnattended || opts.CronJobID != ""
	if !unattended {
		return bossanovav1.HeadlessCapabilityProfile_HEADLESS_CAPABILITY_PROFILE_UNSPECIFIED
	}
	return headlessCapabilityProfileForAutonomousRun(resolvedAgentName, trackerAdapter)
}

// commandTransportTrackers names adapters whose skill preflight uses commands.
var commandTransportTrackers = map[string]struct{}{"github": {}}

// headlessCapabilityProfileForAutonomousRun selects the shared (agent, tracker)
// policy for an already autonomous launch. Only Codex needs an MCP profile;
// known command transports are exempt and unknown trackers retain the gate.
// Agent names must already be resolved through the dispatcher.
func headlessCapabilityProfileForAutonomousRun(resolvedAgentName, trackerAdapter string) bossanovav1.HeadlessCapabilityProfile {
	if _, commandTransport := commandTransportTrackers[trackerAdapter]; resolvedAgentName == agentNameCodex && !commandTransport {
		return bossanovav1.HeadlessCapabilityProfile_HEADLESS_CAPABILITY_PROFILE_TRACKER_PLAN_ATTACHMENT_V1
	}
	return bossanovav1.HeadlessCapabilityProfile_HEADLESS_CAPABILITY_PROFILE_UNSPECIFIED
}

// resolveAgentName reports the agent name this launch will actually route to,
// resolving it exactly the way the dispatcher's by-agent routing does (empty
// name + single loaded runner → that runner, else the configured default
// agent). Agent-keyed launch policy has to key on the same name routing will
// use, or a session with an empty agent name would be judged against "" while
// the run itself went to the default agent.
//
// Dispatchers that cannot report the name (test fakes, future runners) fall
// back to the raw name: a type assertion on a nil interface yields ok=false, so
// this is safe even before an agent runner is wired.
func (l *Lifecycle) resolveAgentName(agentName string) string {
	if resolver, ok := l.agentRunner.(agent.NameResolver); ok {
		return resolver.ResolveAgentName(agentName)
	}
	return agentName
}

// TrackerConfig selects the repository's issue tracker.
type TrackerConfig struct {
	Adapter          string
	GitHubRepository string
}

// ReadTrackerConfig reads the closest readable config, failing closed to Linear
// when that file is malformed or does not select a tracker.
func ReadTrackerConfig(dirs ...string) TrackerConfig {
	fallback := TrackerConfig{Adapter: "linear"}
	for _, dir := range dirs {
		if dir == "" {
			continue
		}
		data, err := os.ReadFile(filepath.Join(dir, ".boss-skills.json"))
		if err != nil {
			continue
		}
		var config struct {
			Adapters struct {
				Tracker json.RawMessage `json:"tracker"`
			} `json:"adapters"`
			TrackerConfig json.RawMessage `json:"trackerConfig"`
		}
		if json.Unmarshal(data, &config) != nil {
			return fallback
		}
		var tracker string
		if json.Unmarshal(config.Adapters.Tracker, &tracker) != nil || tracker == "" {
			return fallback
		}
		result := TrackerConfig{Adapter: tracker}
		var details struct {
			GitHub struct {
				Repository string `json:"repository"`
			} `json:"github"`
		}
		if json.Unmarshal(config.TrackerConfig, &details) == nil {
			result.GitHubRepository = details.GitHub.Repository
		}
		return result
	}
	return fallback
}

func resolveTrackerAdapter(dirs ...string) string { return ReadTrackerConfig(dirs...).Adapter }

func (l *Lifecycle) trackerAdapterForSession(sess *models.Session, repo *models.Repo) string {
	var worktreePath, repoPath string
	if sess != nil {
		worktreePath = sess.WorktreePath
	}
	if repo != nil {
		repoPath = repo.LocalPath
	}
	return resolveTrackerAdapter(worktreePath, repoPath)
}
