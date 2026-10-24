//go:build e2e

package attach

// DisableExternalBrowserForE2E applies the router's browser fixture overrides.
func DisableExternalBrowserForE2E(disable func()) { disable() }
