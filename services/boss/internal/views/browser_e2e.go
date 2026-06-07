//go:build e2e

package views

import "context"

// DisableExternalBrowserForE2E makes fixture-driven TUI runs deterministic and
// prevents tests from opening local browser windows.
func DisableExternalBrowserForE2E() {
	openLoginVerificationURL = func(string) error { return nil }
	openSubscriptionCheckoutURL = func(string) error { return nil }
	openGitHubAppInstallURL = func(string) error { return nil }
	lookupGitHubAppInstallTarget = func(context.Context, string) (githubAppInstallTarget, error) {
		return githubAppInstallTarget{}, nil
	}
}
