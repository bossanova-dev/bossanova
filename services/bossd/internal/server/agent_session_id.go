package server

import (
	"fmt"
	"regexp"
	"strings"
)

// canonicalUUID is the only agent_session_id shape a Codex chat accepts: the
// Codex plugin resolves the rollout transcript by matching this id against a
// filename, so anything looser widens that match.
var canonicalUUID = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

// validateAgentSessionID refuses an agent_session_id that an agent plugin could
// turn into a broader filesystem lookup than the one chat it names: glob
// metacharacters and path separators for every agent, and anything but a
// canonical lowercase UUID for Codex.
func validateAgentSessionID(agentName, id string) error {
	if strings.ContainsAny(id, `*?[]\/`) || strings.Contains(id, "..") {
		return fmt.Errorf("agent_session_id %q contains a path separator or glob metacharacter", id)
	}
	if agentName == "codex" && !canonicalUUID.MatchString(id) {
		return fmt.Errorf("agent_session_id %q for a codex chat must be a canonical lowercase UUID", id)
	}
	return nil
}
