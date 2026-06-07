//go:build e2e

package views

import (
	"errors"
	"testing"
)

func TestDisableExternalBrowserForE2EDisablesLoginAndSubscriptionOpeners(t *testing.T) {
	originalLoginOpen := openLoginVerificationURL
	originalSubscriptionOpen := openSubscriptionCheckoutURL
	originalGitHubAppOpen := openGitHubAppInstallURL
	openLoginVerificationURL = func(string) error {
		return errors.New("login opener should be disabled")
	}
	openSubscriptionCheckoutURL = func(string) error {
		return errors.New("subscription opener should be disabled")
	}
	openGitHubAppInstallURL = func(string) error {
		return errors.New("github app opener should be disabled")
	}
	t.Cleanup(func() {
		openLoginVerificationURL = originalLoginOpen
		openSubscriptionCheckoutURL = originalSubscriptionOpen
		openGitHubAppInstallURL = originalGitHubAppOpen
	})

	DisableExternalBrowserForE2E()

	if err := openLoginVerificationURL("https://auth.example.test/device"); err != nil {
		t.Fatalf("login opener returned error after disable: %v", err)
	}
	if err := openSubscriptionCheckoutURL("https://billing.example.test/checkout"); err != nil {
		t.Fatalf("subscription opener returned error after disable: %v", err)
	}
	if err := openGitHubAppInstallURL("https://github.com/apps/bossanova-dev/installations/new"); err != nil {
		t.Fatalf("github app opener returned error after disable: %v", err)
	}
}
