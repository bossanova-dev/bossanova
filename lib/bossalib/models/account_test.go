package models

import "testing"

func TestAccountIsOutOfService(t *testing.T) {
	const refusal = "account suspended: organization disabled Claude subscription access"
	for _, tc := range []struct {
		name string
		acct *Account
		want bool
	}{
		{"active and healthy", &Account{Status: AccountStatusActive, Health: AccountHealthOK}, false},
		{"disabled by the operator", &Account{Status: AccountStatusDisabled, Health: AccountHealthOK}, true},
		{"suspended by the provider", &Account{Status: AccountStatusActive, Health: AccountHealthFailed, LastTestError: refusal}, true},
		{"failed for another reason", &Account{Status: AccountStatusActive, Health: AccountHealthFailed, LastTestError: "keyring locked"}, false},
		{"refusal text on a healthy account", &Account{Status: AccountStatusActive, Health: AccountHealthOK, LastTestError: refusal}, false},
		{"nil", nil, false},
	} {
		if got := tc.acct.IsOutOfService(); got != tc.want {
			t.Errorf("%s: IsOutOfService() = %v, want %v", tc.name, got, tc.want)
		}
	}
}
