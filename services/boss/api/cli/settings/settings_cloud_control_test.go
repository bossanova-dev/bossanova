package settings

import (
	"bytes"
	"strings"
	"testing"

	"github.com/recurser/bossalib/kit/config"
)

func Test_Settings_AC210501_AC218601_CloudControlFlag(t *testing.T) {
	for _, mode := range []string{"private", "full", "read-only", "secret", ""} {
		t.Run(mode, func(t *testing.T) {
			seedDaemonNameSettings(t)
			seed, err := config.Load()
			if err != nil {
				t.Fatal(err)
			}
			seed.DaemonName = "keep-me"
			if err := config.Save(seed); err != nil {
				t.Fatal(err)
			}
			cmd := newTestGroup().SettingsCmd()
			cmd.SetArgs([]string{"--cloud-control", mode})
			var stderr bytes.Buffer
			cmd.SetErr(&stderr)
			cmd.SetOut(&stderr)
			var executeErr error
			out := captureStdout(t, func() { executeErr = cmd.Execute() })
			stored, err := config.Load()
			if err != nil {
				t.Fatal(err)
			}
			if stored.DaemonName != "keep-me" {
				t.Fatal("unrelated setting changed")
			}
			if mode != "private" && mode != "full" && mode != "read-only" {
				if executeErr == nil || !strings.Contains(executeErr.Error(), "private") || !strings.Contains(executeErr.Error(), "read-only") || !strings.Contains(executeErr.Error(), "full") || !strings.Contains(out+stderr.String(), "Usage:") {
					t.Fatalf("want usage with modes, got %v / %s", executeErr, stderr.String())
				}
				if stored.CloudControl != "" {
					t.Fatalf("invalid mode saved: %q", stored.CloudControl)
				}
				return
			}
			if executeErr != nil {
				t.Fatal(executeErr)
			}
			if stored.CloudControl != mode {
				t.Fatalf("stored %q, want %q", stored.CloudControl, mode)
			}
			warning := "Anyone who can act as you in Boss Cloud can then drive agents on this machine."
			if strings.Contains(out, warning) != (mode == "full") {
				t.Fatalf("unexpected warning output: %s", out)
			}
			if mode == "private" && !strings.Contains(out, "not their transcripts, terminals, notes, cron prompts or callback prompts") {
				t.Fatalf("private explanation missing: %s", out)
			}
			read := newTestGroup().SettingsCmd()
			read.SetArgs([]string{})
			summary := captureStdout(t, func() {
				if err := read.Execute(); err != nil {
					t.Fatal(err)
				}
			})
			if !strings.Contains(summary, "Cloud control:    "+mode) {
				t.Fatalf("missing mode summary: %s", summary)
			}
		})
	}
}

func TestSettingsCloudControlSummary(t *testing.T) {
	for _, tc := range []struct{ raw, want string }{
		{"", "full (default)"}, {"read-only", "read-only"}, {"private", "private"}, {"full", "full"},
		{"future", `read-only (unrecognised value "future")`}, {" full ", "full"},
	} {
		t.Run(tc.raw, func(t *testing.T) {
			seedDaemonNameSettings(t)
			s, err := config.Load()
			if err != nil {
				t.Fatal(err)
			}
			s.CloudControl = tc.raw
			if err := config.Save(s); err != nil {
				t.Fatal(err)
			}
			cmd := newTestGroup().SettingsCmd()
			cmd.SetArgs([]string{})
			out := captureStdout(t, func() {
				if err := cmd.Execute(); err != nil {
					t.Fatal(err)
				}
			})
			if !strings.Contains(out, "Cloud control:    "+tc.want) {
				t.Fatalf("summary %q lacks %q", out, tc.want)
			}
		})
	}
}
