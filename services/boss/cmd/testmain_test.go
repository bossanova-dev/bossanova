package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	pb "github.com/recurser/bossalib/gen/bossanova/v1"
)

// TestMain isolates the whole cmd test package from the developer's real
// settings.json. The config guard (config.Path) refuses the OS-default settings
// path under test, so any test that reaches config.Save/Load without isolating
// would otherwise error. Pointing BOSS_SETTINGS_PATH at a throwaway temp file
// gives every test a writable, disposable settings file by default. Individual
// tests can still t.Setenv("BOSS_SETTINGS_PATH", ...) to a specific path;
// t.Setenv restores this default afterward.
func TestMain(m *testing.M) {
	// Same isolation, second environment leak: `boss daemon doctor` now asks
	// the LIVE daemon for its auth state, so without a default stub every
	// doctor test in this package would render a line whose text depends on
	// whether the developer running the suite happens to be signed in — and on
	// an older local daemon that line even carries a remediation string other
	// tests assert the absence of. Default to "could not reach it"; tests that
	// care override daemonAuthStateProbe themselves.
	daemonAuthStateProbe = func(context.Context) (*pb.GetAuthStateResponse, error) {
		return nil, errors.New("daemon auth probe is stubbed out in this test package")
	}

	dir, err := os.MkdirTemp("", "boss-cmd-settings-*")
	if err != nil {
		panic(err)
	}
	if err := os.Setenv("BOSS_SETTINGS_PATH", filepath.Join(dir, "settings.json")); err != nil {
		panic(err)
	}
	code := m.Run()
	_ = os.RemoveAll(dir)
	os.Exit(code)
}
