package server

import (
	"fmt"
	"strings"

	"github.com/recurser/bossalib/config"
)

// resolveAgentName fills in a blank agent name using the daemon's loaded-plugin
// state and configured default. It is shared by every create path (interactive
// CreateSession and CreateCronJob) so the two can't drift: a session and a cron
// job created with no explicit agent resolve to the same runner.
//
// A non-blank requested name is returned unchanged. Defense in depth against a
// config.Load failure: an empty string returned here is still safe because the
// SQLite stores apply their own ""→"claude" fallback.
func (s *Server) resolveAgentName(requested string) string {
	var loaded []string
	if s.pluginHost != nil {
		loaded = s.pluginHost.AgentClientNames()
	}
	// config.Load backfills DefaultAgent to "claude" when the settings file
	// omits it, so the fallback is non-empty in the common case.
	cfg, _ := config.Load()
	return resolveDefaultAgentName(requested, loaded, cfg.DefaultAgent)
}

// resolveDefaultAgentName is the pure resolution rule, factored out so it can be
// unit-tested without a live plugin host:
//
//   - a non-blank requested name wins;
//   - otherwise, when exactly one runner is loaded, prefer it — this lets a
//     fresh "install codex, no settings" deployment work without editing the
//     settings file, mirroring the dispatcher's single-loaded-agent rule;
//   - otherwise (zero or several runners loaded) fall back to the configured
//     default agent.
func resolveDefaultAgentName(requested string, loaded []string, defaultAgent string) string {
	if strings.TrimSpace(requested) != "" {
		return requested
	}
	if len(loaded) == 1 {
		return loaded[0]
	}
	return defaultAgent
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
