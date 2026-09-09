package main

import (
	"testing"
	"time"

	"github.com/rs/zerolog"

	sharedplugin "github.com/recurser/bossalib/plugin"
)

// TestParentWatchFailsOpenOnStartup is the plan's negative control, run
// against the real production entry point with the real poll interval: a main
// whose parent identity is absent, or present but not describing this process,
// must reach goplugin.Serve rather than exiting during startup. If the
// arm-time guard were wired backwards this test binary would be terminated by
// the watchdog's os.Exit rather than failing an assertion.
func TestParentWatchFailsOpenOnStartup(t *testing.T) {
	t.Run("identity absent", func(t *testing.T) {
		t.Setenv(sharedplugin.ParentPIDEnvVar, "")
		sharedplugin.StartParentWatch(zerolog.Nop())()
	})

	t.Run("identity does not describe this process", func(t *testing.T) {
		t.Setenv(sharedplugin.ParentPIDEnvVar, "999999")
		stop := sharedplugin.StartParentWatch(zerolog.Nop())
		defer stop()
		// Outlive one real poll interval: an armed watcher would fire here.
		time.Sleep(sharedplugin.DefaultParentPollInterval + 500*time.Millisecond)
	})
}
