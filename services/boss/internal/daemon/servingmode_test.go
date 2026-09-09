package daemon

import (
	"runtime"
	"testing"
)

// TestClassifyServingModeMatrix walks the input matrix BOS-1181 named: plist
// present/absent x service-manager job running/claimed-without-a-PID/absent x
// standalone PID recorded/not.
//
// Every verdict here is unchanged by BOS-1218, and that is the point of
// running the table after it: the classifier is pure, so narrowing
// Status.Running upstream must not move a single row. What changed is which
// rows a real host can still produce. The `Running: true` rows with no
// ServiceManagerPID were the BOS-1181 incident state — a launchd job that was
// registered and never spawned reported exactly that — and no platform probe
// emits that shape any more. They stay because ServingFacts is a plain struct
// any caller fills by hand, and a supervised verdict is what routes stop and
// restart onto the service-manager path: a caller that claims Running without
// a PID must still never get one.
func TestClassifyServingModeMatrix(t *testing.T) {
	const jobPID = 4242

	cases := []struct {
		name string
		// facts minus StandaloneSupported, which each case is run under twice.
		facts ServingFacts
		// want is the verdict on a platform that has a standalone serving mode
		// (macOS); wantUnsupported is the verdict where it does not (Linux).
		want            ServingMode
		wantUnsupported ServingMode
	}{
		{
			name:            "nothing installed and nothing running",
			facts:           ServingFacts{},
			want:            ServingModeUnserved,
			wantUnsupported: ServingModeUnserved,
		},
		{
			name:            "no plist, live standalone daemon",
			facts:           ServingFacts{StandalonePID: 27923, StandaloneAlive: true},
			want:            ServingModeStandalone,
			wantUnsupported: ServingModeUnserved,
		},
		{
			name:            "no plist, recorded standalone PID is dead",
			facts:           ServingFacts{StandalonePID: 27923},
			want:            ServingModeUnserved,
			wantUnsupported: ServingModeUnserved,
		},
		{
			// The BOS-1181 incident, as a real host could then produce it:
			// Installed true (a plist exists) and Running true (`launchctl
			// list` exited 0 for a registered job) while launchd had spawned
			// nothing, so no PID. BOS-1218 narrowed Running so that host now
			// reports Running false; the row is kept as the hand-filled
			// caller, and the verdict it must get is unchanged — what is
			// serving is the standalone process recorded for this profile.
			name:            "plist present, Running claimed with no service PID, live standalone daemon",
			facts:           ServingFacts{Installed: true, Running: true, StandalonePID: 27923, StandaloneAlive: true},
			want:            ServingModeStandalone,
			wantUnsupported: ServingModeUnserved,
		},
		{
			name:            "plist present, Running claimed with no service PID, no standalone daemon",
			facts:           ServingFacts{Installed: true, Running: true},
			want:            ServingModeUnserved,
			wantUnsupported: ServingModeUnserved,
		},
		{
			name:            "plist present, Running claimed with no service PID, dead standalone record",
			facts:           ServingFacts{Installed: true, Running: true, StandalonePID: 27923},
			want:            ServingModeUnserved,
			wantUnsupported: ServingModeUnserved,
		},
		{
			// A correctly supervised host: launchd spawned bossd, and that same
			// process recorded its own PID in the profile's daemon state (bossd
			// writes it on every startup, however it was spawned). Reading the
			// record alone would downgrade this host to standalone forever, so
			// the PID identity is what separates the two.
			name:            "plist present, job spawned, daemon state records the launchd PID",
			facts:           ServingFacts{Installed: true, Running: true, ServiceManagerPID: jobPID, StandalonePID: jobPID, StandaloneAlive: true},
			want:            ServingModeSupervised,
			wantUnsupported: ServingModeSupervised,
		},
		{
			name:            "plist present, job spawned, no standalone record",
			facts:           ServingFacts{Installed: true, Running: true, ServiceManagerPID: jobPID},
			want:            ServingModeSupervised,
			wantUnsupported: ServingModeSupervised,
		},
		{
			name:            "plist present, job spawned, stale standalone record from an older process",
			facts:           ServingFacts{Installed: true, Running: true, ServiceManagerPID: jobPID, StandalonePID: 27923},
			want:            ServingModeSupervised,
			wantUnsupported: ServingModeSupervised,
		},
		{
			// Two live daemons for one profile should not happen (bossd holds a
			// singleton lock), but if the record names a different live process
			// than the one launchd spawned, the recorded one is what wrote the
			// state and restart must preserve it rather than the job.
			name:            "plist present, job spawned, a different live standalone daemon is recorded",
			facts:           ServingFacts{Installed: true, Running: true, ServiceManagerPID: jobPID, StandalonePID: 27923, StandaloneAlive: true},
			want:            ServingModeStandalone,
			wantUnsupported: ServingModeSupervised,
		},
		{
			name:            "plist absent but a job is somehow spawned",
			facts:           ServingFacts{Running: true, ServiceManagerPID: jobPID},
			want:            ServingModeSupervised,
			wantUnsupported: ServingModeSupervised,
		},
		{
			name:            "plist present, job not loaded, live standalone daemon",
			facts:           ServingFacts{Installed: true, StandalonePID: 27923, StandaloneAlive: true},
			want:            ServingModeStandalone,
			wantUnsupported: ServingModeUnserved,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			supported := tc.facts
			supported.StandaloneSupported = true
			if got := ClassifyServingMode(supported); got != tc.want {
				t.Fatalf("ClassifyServingMode(%+v) = %q, want %q", supported, got, tc.want)
			}

			unsupported := tc.facts
			unsupported.StandaloneSupported = false
			if got := ClassifyServingMode(unsupported); got != tc.wantUnsupported {
				t.Fatalf("ClassifyServingMode(%+v) = %q, want %q", unsupported, got, tc.wantUnsupported)
			}
		})
	}
}

// TestClassifyServingModeIgnoresPlistPresence is R2 stated as a test: plist
// presence is a fact about a file and must never move the verdict on its own.
func TestClassifyServingModeIgnoresPlistPresence(t *testing.T) {
	bases := []ServingFacts{
		{StandaloneSupported: true},
		{StandaloneSupported: true, Running: true},
		{StandaloneSupported: true, Running: true, ServiceManagerPID: 4242},
		{StandaloneSupported: true, StandalonePID: 27923, StandaloneAlive: true},
		{StandaloneSupported: true, Running: true, StandalonePID: 27923, StandaloneAlive: true},
	}
	for _, base := range bases {
		absent, present := base, base
		absent.Installed, present.Installed = false, true
		if got, want := ClassifyServingMode(present), ClassifyServingMode(absent); got != want {
			t.Fatalf("ClassifyServingMode with plist = %q, without = %q, for %+v: plist presence must not decide the serving mode", got, want, base)
		}
	}
}

// TestStandaloneServingSupportedIsDarwinOnly pins the platform seam. bossd's
// standalone fallback is a macOS concern for BOS-1181; the Linux path must be
// behaviourally unchanged, which it is only while this reports false there.
func TestStandaloneServingSupportedIsDarwinOnly(t *testing.T) {
	if got, want := StandaloneServingSupported(), runtime.GOOS == "darwin"; got != want {
		t.Fatalf("StandaloneServingSupported() = %t on %s, want %t", got, runtime.GOOS, want)
	}
}
