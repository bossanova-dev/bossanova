//go:build e2e

package login

// DisableExternalBrowserForE2E prevents fixture runs from opening browser windows.
func DisableExternalBrowserForE2E() {
	openLoginVerificationURL = func(string) error { return nil }
	openSubscriptionCheckoutURL = func(string) error { return nil }
}
