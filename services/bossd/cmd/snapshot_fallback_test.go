package main

import "testing"

func TestSnapshotFallbackEnabled(t *testing.T) {
	cases := []struct {
		name string
		env  map[string]string
		want bool
	}{
		{"unset", map[string]string{}, false},
		{"explicit true", map[string]string{"BOSSD_SNAPSHOT_FALLBACK": "true"}, true},
		{"explicit false", map[string]string{"BOSSD_SNAPSHOT_FALLBACK": "false"}, false},
		{"junk value", map[string]string{"BOSSD_SNAPSHOT_FALLBACK": "1"}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			getenv := func(k string) string { return tc.env[k] }
			if got := snapshotFallbackEnabled(getenv); got != tc.want {
				t.Fatalf("snapshotFallbackEnabled() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestSnapshotReconcileDisabled(t *testing.T) {
	cases := []struct {
		name string
		env  map[string]string
		want bool
	}{
		{"unset runs by default", map[string]string{}, false},
		{"explicit false disables", map[string]string{"BOSSD_SNAPSHOT_RECONCILE": "false"}, true},
		{"explicit true runs", map[string]string{"BOSSD_SNAPSHOT_RECONCILE": "true"}, false},
		{"junk value runs", map[string]string{"BOSSD_SNAPSHOT_RECONCILE": "0"}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			getenv := func(k string) string { return tc.env[k] }
			if got := snapshotReconcileDisabled(getenv); got != tc.want {
				t.Fatalf("snapshotReconcileDisabled() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestSnapshotIntervalsAreSane(t *testing.T) {
	// Steady-state reconcile must be gentler than the break-glass cadence,
	// so a healthy stream-fed daemon isn't republishing full snapshots every
	// few seconds.
	if steadyStateSnapshotInterval <= snapshotFallbackInterval {
		t.Fatalf("steady-state interval %v must exceed fallback interval %v",
			steadyStateSnapshotInterval, snapshotFallbackInterval)
	}
}
