package daemon

import (
	"errors"
	"runtime"
	"strings"
	"testing"

	"github.com/recurser/bossalib/config"
)

// TestSupervisionModeMatrix walks every settings value BOS-1184 names — absent,
// each recognised mode, spelling variants of them, and unrecognised junk —
// against BOTH platform postures. Each case is run twice, exactly as
// TestClassifyServingModeMatrix does, so macOS's and Linux's behaviour are both
// provable from either platform's test run.
//
// The load-bearing row is "unattended": it must resolve to NO mode on either
// posture, with a DIFFERENT named error each way, and must never quietly become
// launch-agent. A silent fallback there is the specific defect this seam exists
// to make impossible.
func TestSupervisionModeMatrix(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		// want / wantErr are the verdict where a substrate is selectable
		// (macOS); wantUnconfigurable / wantUnconfigurableErr are the verdict
		// where it is not (Linux and everything else).
		want                  SupervisionMode
		wantErr               error
		wantUnconfigurable    SupervisionMode
		wantUnconfigurableErr error
	}{
		{
			name:               "absent key resolves to the default",
			raw:                "",
			want:               SupervisionModeLaunchAgent,
			wantUnconfigurable: SupervisionModeLaunchAgent,
		},
		{
			name:               "whitespace-only key resolves to the default",
			raw:                "   \t ",
			want:               SupervisionModeLaunchAgent,
			wantUnconfigurable: SupervisionModeLaunchAgent,
		},
		{
			name:               "explicit launch-agent",
			raw:                "launch-agent",
			want:               SupervisionModeLaunchAgent,
			wantUnconfigurable: SupervisionModeLaunchAgent,
		},
		{
			name:               "explicit launch-agent tolerates case and padding",
			raw:                "  Launch-Agent ",
			want:               SupervisionModeLaunchAgent,
			wantUnconfigurable: SupervisionModeLaunchAgent,
		},
		{
			// BOS-1184 U3 shipped the mechanism, so on a platform that can
			// select a substrate this now RESOLVES. Off that platform it is
			// still refused, and with the platform error rather than a
			// not-implemented one — the mode exists everywhere, it is only
			// actionable on macOS.
			name:                  "explicit unattended resolves where a substrate is selectable",
			raw:                   "unattended",
			want:                  SupervisionModeUnattended,
			wantUnconfigurableErr: ErrSupervisionModeUnsupportedPlatform,
		},
		{
			name:                  "explicit unattended tolerates case and padding",
			raw:                   " UNATTENDED ",
			want:                  SupervisionModeUnattended,
			wantUnconfigurableErr: ErrSupervisionModeUnsupportedPlatform,
		},
		{
			name:                  "unrecognised value fails closed",
			raw:                   "system-daemon",
			wantErr:               ErrUnknownSupervisionMode,
			wantUnconfigurableErr: ErrUnknownSupervisionMode,
		},
		{
			// A near-miss of the default is the case a fail-OPEN resolver would
			// swallow, silently selecting the substrate the operator was trying
			// to move off.
			name:                  "near-miss of the default fails closed rather than defaulting",
			raw:                   "launchagent",
			wantErr:               ErrUnknownSupervisionMode,
			wantUnconfigurableErr: ErrUnknownSupervisionMode,
		},
	}

	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			for _, posture := range []struct {
				label        string
				configurable bool
				want         SupervisionMode
				wantErr      error
			}{
				{"configurable", true, tt.want, tt.wantErr},
				{"unconfigurable", false, tt.wantUnconfigurable, tt.wantUnconfigurableErr},
			} {
				t.Run(posture.label, func(t *testing.T) {
					mode, err := resolveSupervisionMode(tt.raw, posture.configurable)
					if posture.wantErr != nil {
						if !errors.Is(err, posture.wantErr) {
							t.Fatalf("resolve(%q, configurable=%t) error = %v, want %v", tt.raw, posture.configurable, err, posture.wantErr)
						}
						if mode != "" {
							t.Fatalf("resolve(%q, configurable=%t) returned mode %q alongside an error; a rejected configuration must resolve to NO mode so a caller ignoring the error cannot act on one",
								tt.raw, posture.configurable, mode)
						}
						return
					}
					if err != nil {
						t.Fatalf("resolve(%q, configurable=%t) unexpected error: %v", tt.raw, posture.configurable, err)
					}
					if mode != posture.want {
						t.Fatalf("resolve(%q, configurable=%t) = %q, want %q", tt.raw, posture.configurable, mode, posture.want)
					}
				})
			}
		})
	}
}

// TestResolveSupervisionModeUsesThisPlatform pins the one thing the matrix above
// cannot: that the exported resolver really does supply the build-tagged const
// to resolveSupervisionMode, which the matrix deliberately bypasses. Without
// this the whole matrix could pass over a wrapper that ignored the platform
// entirely.
func TestResolveSupervisionModeUsesThisPlatform(t *testing.T) {
	mode, err := ResolveSupervisionMode("")
	if err != nil || mode != SupervisionModeLaunchAgent {
		t.Fatalf("ResolveSupervisionMode(\"\") = %q, %v; want %q, nil on every platform", mode, err, SupervisionModeLaunchAgent)
	}

	mode, err = ResolveSupervisionMode("unattended")
	if runtime.GOOS == "darwin" {
		if err != nil || mode != SupervisionModeUnattended {
			t.Fatalf("ResolveSupervisionMode(\"unattended\") on darwin = %q, %v; want %q, nil", mode, err, SupervisionModeUnattended)
		}
		return
	}
	if !errors.Is(err, ErrSupervisionModeUnsupportedPlatform) {
		t.Fatalf("ResolveSupervisionMode(\"unattended\") on %s = %v, want %v", runtime.GOOS, err, ErrSupervisionModeUnsupportedPlatform)
	}
	if mode != "" {
		t.Fatalf("ResolveSupervisionMode(\"unattended\") on %s returned mode %q alongside an error", runtime.GOOS, mode)
	}
}

// TestSupervisionModeNeverReachesTheSystemdPath is the R5 / Linux-untouched pin.
//
// It asserts the property rather than the plumbing: on a platform where no
// substrate is selectable, the ONLY mode any settings value can resolve to is
// launch-agent — the token for today's behaviour, which the systemd path never
// reads. Every other value is refused with no mode at all, so no supervision
// mode can reach a systemd code path however settings.json is written.
func TestSupervisionModeNeverReachesTheSystemdPath(t *testing.T) {
	values := []string{
		"", "   ", "launch-agent", "Launch-Agent",
		"unattended", "UNATTENDED", "system-daemon", "launchagent", "gui", "asuser",
	}

	for _, raw := range values {
		mode, err := resolveSupervisionMode(raw, false)
		if err != nil {
			if mode != "" {
				t.Fatalf("settings value %q was refused but still yielded mode %q", raw, mode)
			}
			continue
		}
		if mode != SupervisionModeLaunchAgent {
			t.Fatalf("settings value %q resolved to %q on a platform with no selectable substrate; only %q may ever resolve there, or the systemd path could be altered by this key",
				raw, mode, SupervisionModeLaunchAgent)
		}
	}
}

// TestLoadSupervisionModeStatusReadsSettings covers the settings-reading half:
// the raw value is carried through for reporting, the platform fact is filled
// in, and an unreadable settings file resolves to the default rather than to an
// error (R5 — an unreadable settings.json must not newly break the default
// install path).
func TestLoadSupervisionModeStatusReadsSettings(t *testing.T) {
	original := loadServiceSettings
	t.Cleanup(func() { loadServiceSettings = original })

	t.Run("absent key", func(t *testing.T) {
		loadServiceSettings = func() (config.Settings, error) { return config.Settings{}, nil }
		st := LoadSupervisionModeStatus()
		if st.Err != nil {
			t.Fatalf("unexpected error: %v", st.Err)
		}
		if st.Mode != SupervisionModeLaunchAgent {
			t.Fatalf("Mode = %q, want %q", st.Mode, SupervisionModeLaunchAgent)
		}
		if st.Configured != "" {
			t.Fatalf("Configured = %q, want empty", st.Configured)
		}
		if st.Configurable != supervisionModeConfigurable {
			t.Fatalf("Configurable = %t, want %t", st.Configurable, supervisionModeConfigurable)
		}
	})

	t.Run("configured value is carried through for reporting", func(t *testing.T) {
		loadServiceSettings = func() (config.Settings, error) {
			return config.Settings{DaemonSupervisionMode: "  Unattended "}, nil
		}
		st := LoadSupervisionModeStatus()
		if st.Configured != "Unattended" {
			t.Fatalf("Configured = %q, want the trimmed raw value so a report can name what the operator wrote", st.Configured)
		}
		if supervisionModeConfigurable {
			if st.Err != nil || st.Mode != SupervisionModeUnattended {
				t.Fatalf("Mode, Err = %q, %v; want %q, nil where a substrate is selectable", st.Mode, st.Err, SupervisionModeUnattended)
			}
			return
		}
		if st.Err == nil {
			t.Fatal("expected the unattended mode to fail closed where no substrate is selectable")
		}
		if st.Mode != "" {
			t.Fatalf("Mode = %q, want empty alongside an error", st.Mode)
		}
	})

	t.Run("a rejected value carries no mode", func(t *testing.T) {
		loadServiceSettings = func() (config.Settings, error) {
			return config.Settings{DaemonSupervisionMode: "system-daemon"}, nil
		}
		st := LoadSupervisionModeStatus()
		if !errors.Is(st.Err, ErrUnknownSupervisionMode) {
			t.Fatalf("Err = %v, want ErrUnknownSupervisionMode", st.Err)
		}
		if st.Mode != "" {
			t.Fatalf("Mode = %q, want empty alongside an error", st.Mode)
		}
		if st.Unattended.State != UnattendedInstallNotApplicable {
			t.Fatalf("Unattended.State = %d, want NotApplicable: no observation may be made for a mode that did not resolve", st.Unattended.State)
		}
	})

	t.Run("the default mode makes no unattended observation", func(t *testing.T) {
		// R5: a host that has not set the key must do exactly what it did
		// before U3, including performing no filesystem observation of a
		// substrate it is not on.
		loadServiceSettings = func() (config.Settings, error) { return config.Settings{}, nil }
		st := LoadSupervisionModeStatus()
		if st.Unattended.State != UnattendedInstallNotApplicable {
			t.Fatalf("Unattended.State = %d on the default substrate, want NotApplicable", st.Unattended.State)
		}
	})

	t.Run("unreadable settings resolve to the default and carry the reason", func(t *testing.T) {
		readErr := errors.New("settings.json is unreadable")
		loadServiceSettings = func() (config.Settings, error) {
			return config.Settings{}, readErr
		}
		st := LoadSupervisionModeStatus()
		if st.Err != nil {
			t.Fatalf("an unreadable settings file must not become a supervision-mode rejection: %v", st.Err)
		}
		if st.Mode != SupervisionModeLaunchAgent {
			t.Fatalf("Mode = %q, want %q", st.Mode, SupervisionModeLaunchAgent)
		}
		// The reason must survive the fallback. Discarding it is what let both
		// reporting surfaces print an affirmative "launch-agent (the default)"
		// for a settings file that names nothing at all.
		if !errors.Is(st.SettingsErr, readErr) {
			t.Fatalf("SettingsErr = %v, want the settings read failure carried out for the reporting surfaces", st.SettingsErr)
		}
	})
}

// TestSupervisionModeRejectionNamesTheValueAndTheAlternatives pins the message
// content the acceptance criterion asks for: an unrecognised value fails with a
// NAMED error that says which value was rejected and which ones exist. An error
// that says only "invalid mode" leaves an operator guessing at the spelling.
func TestSupervisionModeRejectionNamesTheValueAndTheAlternatives(t *testing.T) {
	_, err := parseSupervisionMode("system-daemon")
	if err == nil {
		t.Fatal("expected an error")
	}
	message := err.Error()
	for _, want := range []string{
		"system-daemon",
		string(SupervisionModeLaunchAgent),
		string(SupervisionModeUnattended),
	} {
		if !strings.Contains(message, want) {
			t.Fatalf("rejection %q does not name %q", message, want)
		}
	}
}

// TestSupervisionModeAvailabilityLeavesNoUnreachableRefusal pins the shape U3
// replaced: on a platform that can select a substrate, EVERY recognised mode is
// available, and the only refusal left is the platform one.
//
// It walks recognisedSupervisionModes rather than naming the two values, so a
// third mode added without an implementation trips here instead of shipping a
// refusal an operator can read but not act on.
func TestSupervisionModeAvailabilityLeavesNoUnreachableRefusal(t *testing.T) {
	for _, mode := range recognisedSupervisionModes {
		if err := supervisionModeAvailability(mode, true); err != nil {
			t.Errorf("mode %q is recognised but refused on a host that can select a substrate: %v", mode, err)
		}
		if mode == SupervisionModeLaunchAgent {
			// The default is the token for "today's substrate" and stays
			// available everywhere; nothing on the systemd path reads it.
			continue
		}
		err := supervisionModeAvailability(mode, false)
		if !errors.Is(err, ErrSupervisionModeUnsupportedPlatform) {
			t.Errorf("mode %q off a substrate-selecting platform = %v, want ErrSupervisionModeUnsupportedPlatform", mode, err)
		}
		if !strings.Contains(err.Error(), "loginctl enable-linger") {
			t.Errorf("platform refusal %q does not say why Linux needs no alternative substrate", err)
		}
	}
}
