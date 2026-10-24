//go:build !e2e

package attach

// DisableExternalBrowserForE2E leaves production browser behavior unchanged.
func DisableExternalBrowserForE2E(func()) {}
