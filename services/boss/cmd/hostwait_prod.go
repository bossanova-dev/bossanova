//go:build !e2e

package main

// runE2EHostReconnectSeed is a no-op in production builds: nothing in a
// production boss stages this screen, and a real dropped tunnel surfaces on the
// home screen's daemon-down view instead. The e2e-tagged variant in
// hostwait_e2e.go reads BOSS_HOST_E2E_RECONNECT so a proof scenario can stage
// the wait screen without a second machine. Keeping the env read behind the
// build tag means a production boss cannot be talked into a blocking wait
// screen by an environment variable.
func runE2EHostReconnectSeed() error {
	return nil
}
