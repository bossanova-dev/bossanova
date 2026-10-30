package agentruntime_test

import (
	"path/filepath"
	"testing"

	"github.com/rs/zerolog"

	"github.com/recurser/bossalib/contract/plugin/agentruntime"
)

func TestRunnerBuildArgvReceivesLaunchEnv(t *testing.T) {
	dir := t.TempDir()
	var got string
	r := agentruntime.NewRunner(zerolog.Nop(),
		agentruntime.Options{
			BuildArgv: func(in agentruntime.BuildArgvInput) []string {
				got = in.ExtraEnv["ARGV_ENV_PROOF"]
				return []string{"agent"}
			},
			BinaryName: "fake",
		},
		agentruntime.WithCommandFactory(fakeCmd(t, `printf '%s\n' "$ARGV_ENV_PROOF"`)),
	)
	runnerLogTexts(t, r, dir, filepath.Join(dir, "agent.log"), "argv-env", map[string]string{"ARGV_ENV_PROOF": "launch-env-proof"})
	if got != "launch-env-proof" {
		t.Fatalf("argv builder launch environment = %q, want launch-env-proof", got)
	}
}
