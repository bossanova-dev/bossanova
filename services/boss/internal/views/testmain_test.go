package views

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/recurser/bossalib/termnorm"
)

// TestMain isolates the whole views test package from the developer's real
// settings.json. The config write guard refuses the OS-default settings path —
// and the start-of-process BOSS_SETTINGS_PATH — under test, so any test that
// reaches config.Save/Load without isolating would otherwise error. Pointing
// BOSS_SETTINGS_PATH at a throwaway temp file gives every test a writable,
// disposable settings file by default; withTempConfigHome still overrides it
// per-test where finer isolation is needed (t.Setenv restores this default
// afterward). Mirrors services/boss/cmd/testmain_test.go.
func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "boss-views-settings-*")
	if err != nil {
		panic(err)
	}
	if err := os.Setenv("BOSS_SETTINGS_PATH", filepath.Join(dir, "settings.json")); err != nil {
		panic(err)
	}
	// Same isolation rule, applied to the network. Any test that drives the
	// attach model to chatRecordedMsg under a --host destination reaches
	// attachTmuxEnv, and the real prober would spawn an outbound
	// `ssh deploy@bastion.example.com …` — which fails only on DNS, so it looks
	// fine until a resolver that wildcards NXDOMAIN stalls it to the probe's
	// full bound. Defaulting here rather than inside a per-test helper keeps it
	// free of ordering coupling: a test that installs its own prober simply
	// overrides this one and restores back to it.
	//
	// The answer mirrors the real probers — absent for an empty TERM (no host
	// has an entry under that name), fail open otherwise — so it can never
	// silently rewrite a TERM in a test that is about something else.
	newRemoteTerminfoProber = func(string) termnorm.Prober {
		return func(term string) bool { return term != "" }
	}
	code := m.Run()
	_ = os.RemoveAll(dir)
	os.Exit(code)
}
