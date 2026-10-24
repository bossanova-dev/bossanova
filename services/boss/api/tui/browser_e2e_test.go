//go:build e2e

package tui

import (
	"errors"
	"testing"
)

func TestDisableExternalBrowserForE2EDisablesLoginAndSubscriptionOpeners(t *testing.T) {
	originalGitHubAppOpen := openGitHubAppInstallURL
	openGitHubAppInstallURL = func(string) error {
		return errors.New("github app opener should be disabled")
	}
	t.Cleanup(func() {
		openGitHubAppInstallURL = originalGitHubAppOpen
	})

	DisableExternalBrowserForE2E()

	if err := openGitHubAppInstallURL("https://github.com/apps/bossanova-dev/installations/new"); err != nil {
		t.Fatalf("github app opener returned error after disable: %v", err)
	}
}
