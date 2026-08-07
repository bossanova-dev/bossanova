//go:build !e2e

package main

// applyE2EHostAttachSeed is a no-op in production builds: the only thing that
// puts a production boss into a remote context is a real --host startup, which
// sets the destination itself in cmd/host.go. The e2e-tagged variant in
// hostattach_e2e.go reads BOSS_HOST_E2E_ATTACH_DESTINATION so a proof scenario
// can capture the --host-degraded views without a second machine. Keeping the
// env read behind the build tag means no environment variable can make a
// production boss attach over ssh to a host the user never named.
// It returns "" (no destination applied) to match the e2e signature.
func applyE2EHostAttachSeed() string { return "" }
