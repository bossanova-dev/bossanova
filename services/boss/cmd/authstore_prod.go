//go:build !e2e

package main

import "github.com/recurser/boss/internal/auth"

// resolveE2ETokenStore returns a test-only TokenStore override, or nil in
// production builds. The e2e-tagged variant in authstore_e2e.go reads the
// BOSS_AUTH_E2E_EMAIL env var so TUI integration tests can seed a logged-in
// user without touching the OS keychain.
func resolveE2ETokenStore() auth.TokenStore {
	return nil
}
