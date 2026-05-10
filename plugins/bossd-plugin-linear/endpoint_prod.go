//go:build !e2e

package main

// resolveLinearEndpoint returns the production Linear GraphQL endpoint.
// Production builds compile this variant, so the shipped plugin binary never
// reads LINEAR_API_ENDPOINT from the environment. The e2e-tagged variant in
// endpoint_e2e.go honours the env var for integration tests.
func resolveLinearEndpoint() string {
	return defaultLinearEndpoint
}
