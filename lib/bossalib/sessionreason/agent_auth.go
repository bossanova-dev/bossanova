package sessionreason

// agentAuthFailed is the stable, machine-matchable blocked reason for a session
// whose agent chat shows the login-required terminal shape. It is short and
// slug-shaped on purpose: the daemon persists it on a stranded unattended run it
// blocks instead of finalizing (BOS-1530), and the server's attention overlay
// stamps the same value on the live projection, so both read as one reason.
const agentAuthFailed = "agent-auth-failed"

// AgentAuthFailed returns the blocked reason for a session whose agent chat is
// auth-failed (login required).
func AgentAuthFailed() string {
	return agentAuthFailed
}

// IsAgentAuthFailed reports whether a blocked reason was produced by
// AgentAuthFailed.
func IsAgentAuthFailed(reason *string) bool {
	return reason != nil && *reason == agentAuthFailed
}
