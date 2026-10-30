package sessionreason

const agentProviderError = "agent-provider-error"

// AgentProviderError returns the blocked reason for a run that ended on a provider error.
func AgentProviderError() string { return agentProviderError }

// IsAgentProviderError reports whether reason identifies a provider-error run.
func IsAgentProviderError(reason *string) bool {
	return reason != nil && *reason == agentProviderError
}
