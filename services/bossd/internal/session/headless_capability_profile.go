package session

import (
	bossanovav1 "github.com/recurser/bossalib/gen/bossanova/v1"

	"github.com/recurser/bossd/internal/agent"
)

// agentNameCodex is the plugin name of the Codex agent runner, as persisted on
// a session and keyed in the dispatcher's runner registry.
const agentNameCodex = "codex"

// headlessCapabilityProfileFor reports the headless operation surface an
// unattended launch of resolvedAgentName requires. It is pure: it reads only
// the launch signals already on opts and mutates nothing.
//
// A launch is unattended when it carries any of the three markers that mean
// "no human is watching this pass": Detach (headless `boss new --detach`),
// IsTmuxUnattended (the durable tmux-hosted path), or a CronJobID. ZeroOutput
// is not an interactivity marker — a zero-output cron fire is still unattended.
//
// Only Codex serves the profile preflight, so only an unattended Codex launch
// requires a profile. Interactive launches, every Claude launch, and an
// unresolved (empty) agent name return UNSPECIFIED byte-for-byte, leaving their
// launch path exactly as it was — an unresolved name must never opt a launch
// into a gate its runner may not implement.
//
// resolvedAgentName must already be resolved the way the dispatcher resolves it
// (see agent.AgentNameResolver); this function does no fallback of its own.
func headlessCapabilityProfileFor(resolvedAgentName string, opts StartSessionOpts) bossanovav1.HeadlessCapabilityProfile {
	unattended := opts.Detach || opts.IsTmuxUnattended || opts.CronJobID != ""
	if !unattended {
		return bossanovav1.HeadlessCapabilityProfile_HEADLESS_CAPABILITY_PROFILE_UNSPECIFIED
	}
	return headlessCapabilityProfileForAutonomousRun(resolvedAgentName)
}

// headlessCapabilityProfileForAutonomousRun reports the operation surface an
// autonomous run of resolvedAgentName requires. It is the agent-keyed half of
// the launch policy, shared by every enforcement point so "which agents need a
// profile" is written exactly once.
//
// The caller has already established that the run is autonomous; this function
// answers only the agent question. Callers that also hold interactivity signals
// (headlessCapabilityProfileFor) test those first.
//
// resolvedAgentName must already be resolved the way the dispatcher resolves it
// (see resolveAgentName); this function does no fallback of its own.
func headlessCapabilityProfileForAutonomousRun(resolvedAgentName string) bossanovav1.HeadlessCapabilityProfile {
	if resolvedAgentName == agentNameCodex {
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
	if resolver, ok := l.agentRunner.(agent.AgentNameResolver); ok {
		return resolver.ResolveAgentName(agentName)
	}
	return agentName
}
