package server

import (
	"fmt"
	"strings"

	"github.com/recurser/bossalib/agentsession"
)

// validateAgentSessionID refuses an agent_session_id that an agent plugin could
// turn into a broader filesystem lookup than the one chat it names: glob
// metacharacters and path separators for every agent, and anything but a
// canonical lowercase UUID for Claude and Codex.
func validateAgentSessionID(agentName, id string) error {
	if strings.ContainsAny(id, `*?[]\/`) || strings.Contains(id, "..") {
		return fmt.Errorf("agent_session_id %q contains a path separator or glob metacharacter", id)
	}
	if (agentName == "codex" || agentName == "claude") && !agentsession.IsCanonicalUUID(id) {
		return fmt.Errorf("agent_session_id %q for a %s chat must be a canonical lowercase UUID", id, agentName)
	}
	return nil
}
