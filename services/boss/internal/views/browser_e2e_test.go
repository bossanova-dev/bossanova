//go:build e2e

package views

import (
	"errors"
	"testing"
)

func TestDisableExternalBrowserForE2EDisablesLoginAndSubscriptionOpeners(t *testing.T) {
	originalLoginOpen := openLoginVerificationURL
	originalSubscriptionOpen := openSubscriptionCheckoutURL
	openLoginVerificationURL = func(string) error {
		return errors.New("login opener should be disabled")
	}
	openSubscriptionCheckoutURL = func(string) error {
		return errors.New("subscription opener should be disabled")
	}
	t.Cleanup(func() {
		openLoginVerificationURL = originalLoginOpen
		openSubscriptionCheckoutURL = originalSubscriptionOpen
	})

	DisableExternalBrowserForE2E()

	if err := openLoginVerificationURL("https://auth.example.test/device"); err != nil {
		t.Fatalf("login opener returned error after disable: %v", err)
	}
	if err := openSubscriptionCheckoutURL("https://billing.example.test/checkout"); err != nil {
		t.Fatalf("subscription opener returned error after disable: %v", err)
	}
}
