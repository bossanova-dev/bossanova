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
