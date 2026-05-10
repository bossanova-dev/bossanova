//go:build e2e

package main

import "testing"

// TestResolveLinearEndpoint_EnvOverride verifies the e2e-only env-var hook
// wired up by endpoint_e2e.go. Only runs under `go test -tags e2e` because
// production builds intentionally omit the override.
func TestResolveLinearEndpoint_EnvOverride(t *testing.T) {
	t.Setenv("LINEAR_API_ENDPOINT", "http://127.0.0.1:9999/graphql")
	client := newLinearClient("test-key")
	if client.endpoint != "http://127.0.0.1:9999/graphql" {
		t.Errorf("endpoint = %q, want override", client.endpoint)
	}
}
