package domain

import (
	"strings"
)

const InjectionFailureReasonPrefix = "credential injection failed: "

func IsSelfClearingInjectionFailure(a *AgentAccount) bool {
	return a != nil &&
		a.Health == AgentAccountHealthFailed &&
		strings.HasPrefix(a.LastTestError, InjectionFailureReasonPrefix)
}
