package config

import (
	"runtime"
	"strings"
	"testing"
)

// probeReturning builds a computer-name probe that always reports the same name,
// so every precedence case below is platform-independent. The probe-failed case
// has its own helper below, which is why this one takes no ok flag.
func probeReturning(name string) func() (string, bool) {
	return func() (string, bool) { return name, true }
}

// probeUnavailable stands in for every way the real probe can fail: a non-darwin
// build, scutil missing, a non-zero exit, or the timeout firing.
func probeUnavailable() (string, bool) { return "", false }

func TestDefaultDisplayHostnameWith(t *testing.T) {
	tests := []struct {
		name     string
		hostname string
		probe    func() (string, bool)
		want     string
	}{
		{
			name:     "probe result wins over the stripped hostname",
			hostname: "mac.lan",
			probe:    probeReturning("kamikai"),
			want:     "kamikai",
		},
		{
			name:     "probe result is trimmed",
			hostname: "mac.lan",
			probe:    probeReturning("  kamikai  "),
			want:     "kamikai",
		},
		{
			name:     "an empty probe result is not a name",
			hostname: "mac.lan",
			probe:    probeReturning(""),
			want:     "mac",
		},
		{
			name:     "a whitespace-only probe result is not a name",
			hostname: "mac.lan",
			probe:    probeReturning("   "),
			want:     "mac",
		},
		{
			name:     "a failed probe falls back to the stripped hostname",
			hostname: "mac.lan",
			probe:    probeUnavailable,
			want:     "mac",
		},
		{
			name:     "a dot-local hostname is stripped too",
			hostname: "mac.local",
			probe:    probeUnavailable,
			want:     "mac",
		},
		{
			name:     "the suffix match is case-insensitive and preserves the remainder's case",
			hostname: "MAC.LAN",
			probe:    probeUnavailable,
			want:     "MAC",
		},
		{
			name:     "mixed-case dot-Local is stripped and the remainder kept byte for byte",
			hostname: "Studio-iMac.Local",
			probe:    probeUnavailable,
			want:     "Studio-iMac",
		},
		{
			name:     "exactly one suffix is stripped, not greedily",
			hostname: "mac.lan.lan",
			probe:    probeUnavailable,
			want:     "mac.lan",
		},
		{
			name:     "a suffix-only hostname is never emptied out",
			hostname: ".lan",
			probe:    probeUnavailable,
			want:     ".lan",
		},
		{
			name:     "a dot-local-only hostname is never emptied out",
			hostname: ".local",
			probe:    probeUnavailable,
			want:     ".local",
		},
		{
			name:     "an empty hostname stays empty",
			hostname: "",
			probe:    probeUnavailable,
			want:     "",
		},
		{
			name:     "a plain hostname is untouched",
			hostname: "build-server-01",
			probe:    probeUnavailable,
			want:     "build-server-01",
		},
		{
			name:     "an unrelated domain suffix is not stripped",
			hostname: "host.internal.example.com",
			probe:    probeUnavailable,
			want:     "host.internal.example.com",
		},
		{
			name:     "a hostname merely containing .lan mid-string is untouched",
			hostname: "mac.lan.example.com",
			probe:    probeUnavailable,
			want:     "mac.lan.example.com",
		},
		{
			name:     "the probe still wins for a hostname that needs no strip",
			hostname: "build-server-01",
			probe:    probeReturning("Dave's iMac"),
			want:     "Dave's iMac",
		},
		{
			name:     "an empty hostname still takes a usable probe result",
			hostname: "",
			probe:    probeReturning("kamikai"),
			want:     "kamikai",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := defaultDisplayHostnameWith(tc.hostname, tc.probe); got != tc.want {
				t.Fatalf("defaultDisplayHostnameWith(%q) = %q, want %q", tc.hostname, got, tc.want)
			}
		})
	}
}

// TestDefaultDisplayHostnameNeverEmptiesANonEmptyHostname is R2 stated directly
// rather than as a scattering of table rows: bosso rejects a registration whose
// hostname is blank, so no input the daemon can actually hold may derive to "".
func TestDefaultDisplayHostnameNeverEmptiesANonEmptyHostname(t *testing.T) {
	hostnames := []string{
		"mac.lan", "mac.local", ".lan", ".local", ".LAN", "a.lan", "a",
		"build-server-01", "host.internal.example.com", "mac.lan.lan",
	}
	for _, hostname := range hostnames {
		if got := defaultDisplayHostnameWith(hostname, probeUnavailable); got == "" {
			t.Errorf("defaultDisplayHostnameWith(%q) emptied a non-empty hostname", hostname)
		}
		// The exported entry point runs the host's real probe, which on darwin
		// can answer with anything; it must still never yield a blank.
		if got := DefaultDisplayHostname(hostname); got == "" {
			t.Errorf("DefaultDisplayHostname(%q) emptied a non-empty hostname", hostname)
		}
	}
}

// TestDefaultDisplayHostnameExportedSmoke pins the seam-free entry point: it must
// wire the platform probe in without panicking and without inventing a name for
// an input that has none.
func TestDefaultDisplayHostnameExportedSmoke(t *testing.T) {
	if got := DefaultDisplayHostname(""); got != "" {
		// On darwin an empty hostname legitimately resolves to the ComputerName.
		if runtime.GOOS != "darwin" {
			t.Fatalf("DefaultDisplayHostname(%q) = %q, want the empty string on %s", "", got, runtime.GOOS)
		}
	}
	if got := DefaultDisplayHostname("build-server-01"); strings.TrimSpace(got) == "" {
		t.Fatalf("DefaultDisplayHostname(%q) = %q, want a non-blank name", "build-server-01", got)
	}
}

// TestDefaultDisplayHostnameNonDarwinProbeReportsNoName pins R5 on every build
// that is not darwin: the fallback is the suffix strip and nothing else, with no
// subprocess involved.
func TestDefaultDisplayHostnameNonDarwinProbeReportsNoName(t *testing.T) {
	if runtime.GOOS == "darwin" {
		t.Skip("darwin has a real ComputerName probe; the !darwin build is pinned here")
	}
	name, ok := computerName()
	if ok || name != "" {
		t.Fatalf("computerName() = (%q, %v) on %s, want (\"\", false)", name, ok, runtime.GOOS)
	}
	if got := DefaultDisplayHostname("mac.lan"); got != "mac" {
		t.Fatalf("DefaultDisplayHostname(%q) = %q, want the stripped hostname on %s", "mac.lan", got, runtime.GOOS)
	}
}

// TestComputerNameProbeDarwin exercises the real scutil probe. It cannot assert a
// value — every host answers differently, and a CI runner may answer not at all —
// so it pins the contract instead: the probe never panics, and a reported name is
// never blank or untrimmed.
func TestComputerNameProbeDarwin(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skipf("scutil --get ComputerName is darwin-only; GOOS is %s", runtime.GOOS)
	}
	name, ok := computerName()
	if !ok {
		if name != "" {
			t.Fatalf("computerName() reported ok == false with a name %q, want the empty string", name)
		}
		t.Skip("this host reports no ComputerName; the fallback path is what runs here")
	}
	if strings.TrimSpace(name) == "" {
		t.Fatalf("computerName() = (%q, true), want a non-blank trimmed name", name)
	}
	if name != strings.TrimSpace(name) {
		t.Fatalf("computerName() = %q, want it trimmed of surrounding whitespace", name)
	}
	// A usable probe result is exactly what DefaultDisplayHostname advertises,
	// ahead of any suffix stripping.
	if got := DefaultDisplayHostname("mac.lan"); got != name {
		t.Fatalf("DefaultDisplayHostname(%q) = %q, want the probed ComputerName %q", "mac.lan", got, name)
	}
}

// TestMemoizeProbeForksAtMostOnce pins the property the boss TUI depends on: the
// darwin probe forks scutil, and General Settings constructs its model inside
// Bubble Tea's Update, so an unmemoized probe would fork on the render goroutine
// on every navigation into the view.
func TestMemoizeProbeForksAtMostOnce(t *testing.T) {
	calls := 0
	probe := memoizeProbe(func() (string, bool) {
		calls++
		return "kamikai", true
	})

	for i := 0; i < 3; i++ {
		name, ok := probe()
		if name != "kamikai" || !ok {
			t.Fatalf("call %d = (%q, %v), want (%q, true)", i, name, ok, "kamikai")
		}
	}
	if calls != 1 {
		t.Fatalf("underlying probe ran %d times, want exactly 1", calls)
	}
}

// TestMemoizeProbeCachesAFailedProbeToo pins the negative answer as cached as
// well: a host with no scutil must not re-fork on every call just because the
// first answer was "no name".
func TestMemoizeProbeCachesAFailedProbeToo(t *testing.T) {
	calls := 0
	probe := memoizeProbe(func() (string, bool) {
		calls++
		return "", false
	})

	for i := 0; i < 3; i++ {
		if name, ok := probe(); name != "" || ok {
			t.Fatalf("call %d = (%q, %v), want (\"\", false)", i, name, ok)
		}
	}
	if calls != 1 {
		t.Fatalf("underlying probe ran %d times, want exactly 1", calls)
	}
}
