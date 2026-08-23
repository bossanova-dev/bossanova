//go:build e2e

package views

import "os"

// proofSettingsSaveFailure lets TUI proof scenarios exercise failed-save
// rendering without forwarding arbitrary settings paths into the subprocess.
func proofSettingsSaveFailure() bool {
	return os.Getenv("BOSS_PROOF_SETTINGS_SAVE_FAILURE") == "1"
}
