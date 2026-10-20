package models

import "testing"

func TestAccountIsOutOfService(t *testing.T) {
	const refusal = "account suspended: organization disabled Claude subscription access"
	for _, tc := range []struct {
		name string
		acct *AgentAccount
		want bool
	}{
		{"active and healthy", &AgentAccount{Status: AgentAccountStatusActive, Health: AgentAccountHealthOK}, false},
		{"disabled by the operator", &AgentAccount{Status: AgentAccountStatusDisabled, Health: AgentAccountHealthOK}, true},
		{"suspended by the provider", &AgentAccount{Status: AgentAccountStatusActive, Health: AgentAccountHealthFailed, LastTestError: refusal}, true},
		{"failed for another reason", &AgentAccount{Status: AgentAccountStatusActive, Health: AgentAccountHealthFailed, LastTestError: "keyring locked"}, false},
		{"refusal text on a healthy account", &AgentAccount{Status: AgentAccountStatusActive, Health: AgentAccountHealthOK, LastTestError: refusal}, false},
		{"nil", nil, false},
	} {
		if got := tc.acct.IsOutOfService(); got != tc.want {
			t.Errorf("%s: IsOutOfService() = %v, want %v", tc.name, got, tc.want)
		}
	}
}
