//go:build e2e

package main

import "os"

// resolveLinearEndpoint returns the Linear GraphQL endpoint, preferring the
// LINEAR_API_ENDPOINT env var when set. Only compiled into builds tagged
// `e2e` — the integration harness in services/bossd/internal/plugin builds
// the plugin binary with -tags e2e and sets the env var via t.Setenv so
// requests land on a local httptest.Server instead of api.linear.app.
func resolveLinearEndpoint() string {
	if v := os.Getenv("LINEAR_API_ENDPOINT"); v != "" {
		return v
	}
	return defaultLinearEndpoint
}
