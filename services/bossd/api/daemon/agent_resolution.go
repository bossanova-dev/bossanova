package server

import (
	"fmt"
	"slices"
	"strings"

	"github.com/recurser/bossalib/kit/config"
)

// loadedAgentNames returns, sorted, the names of the agent runners this daemon
// can start. s.agentClients is the registry every start path dispatches
// through (and what ListAgents advertises), so it is the source of truth; the
// plugin host is consulted only when no registry was injected.
func (s *Server) loadedAgentNames() []string {
	if len(s.agentClients) > 0 {
		names := make([]string, 0, len(s.agentClients))
		for name := range s.agentClients {
			names = append(names, name)
		}
		slices.Sort(names)
		return names
	}
	if s.pluginHost != nil {
		return s.pluginHost.AgentClientNames()
	}
	return nil
}

// resolveAgentName fills in a blank agent name using the daemon's loaded-plugin
// state and configured default. It is shared by every create path (interactive
// CreateSession and CreateCronJob) so the two can't drift: a session and a cron
// job created with no explicit agent resolve to the same runner.
//
// A non-blank requested name is returned unchanged. Defense in depth against a
// config.Load failure: an empty string returned here is still safe because the
// SQLite stores apply their own ""→"claude" fallback.
func (s *Server) resolveAgentName(requested string) (string, error) {
	// config.Load backfills DefaultAgent to "claude" when the settings file
	// omits it, so the fallback is non-empty in the common case.
	cfg, _ := config.Load()
	return resolveDefaultAgentName(requested, s.loadedAgentNames(), cfg.DefaultAgent)
}

// resolveDefaultAgentName is the pure resolution rule, factored out so it can be
// unit-tested without a live plugin host:
//
//   - a non-blank requested name wins;
//   - otherwise, when exactly one runner is loaded, prefer it — this lets a
//     fresh "install codex, no settings" deployment work without editing the
//     settings file, mirroring the dispatcher's single-loaded-agent rule;
//   - otherwise fall back to the configured default agent, unless several
//     runners are loaded and the default is not one of them. A session stored
//     with an unloaded agent can never start, so that case is refused here,
//     where the caller can still pick an agent, rather than at spawn time.
func resolveDefaultAgentName(requested string, loaded []string, defaultAgent string) (string, error) {
	if strings.TrimSpace(requested) != "" {
		return requested, nil
	}
	if len(loaded) == 1 {
		return loaded[0], nil
	}
	if len(loaded) == 0 || slices.Contains(loaded, defaultAgent) {
		return defaultAgent, nil
	}
	return "", fmt.Errorf("default agent %q is not loaded (loaded agents: %s); pass an agent or change default_agent in settings",
		defaultAgent, strings.Join(loaded, ", "))
}

func (s *Server) validateExplicitAgentName(name string) error {
	name = strings.TrimSpace(name)
	if name == "" {
		return nil
	}
	if len(s.agentClients) > 0 {
		if _, ok := s.agentClients[name]; ok {
			return nil
		}
		return fmt.Errorf("agent %q is not loaded", name)
	}
	if s.pluginHost != nil {
		for _, loaded := range s.pluginHost.AgentClientNames() {
			if loaded == name {
				return nil
			}
		}
		return fmt.Errorf("agent %q is not loaded", name)
	}
	return nil
}
