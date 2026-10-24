//go:build e2e

package tui

import (
	"context"

	"github.com/recurser/boss/api/tui/attach"
	"github.com/recurser/boss/api/tui/login"
)

// DisableExternalBrowserForE2E makes fixture-driven TUI runs deterministic and
// prevents tests from opening local browser windows.
func DisableExternalBrowserForE2E() {
	attach.DisableExternalBrowserForE2E(func() {
		login.DisableExternalBrowserForE2E()
		openGitHubAppInstallURL = func(string) error { return nil }
		lookupGitHubAppInstallTarget = func(context.Context, string) (githubAppInstallTarget, error) {
			return githubAppInstallTarget{}, nil
		}
	})
}
