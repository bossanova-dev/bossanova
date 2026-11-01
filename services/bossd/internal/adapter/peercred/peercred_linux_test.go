//go:build linux

package peercred

import "testing"

func TestParseParentPID(t *testing.T) {
	for _, tc := range []struct {
		name, stat string
		want       int
		wantErr    bool
	}{
		{"normal", "123 (boss) S 42 1 2 3", 42, false},
		{"nested command", "123 (boss (agent) shell)) S 73 1 2", 73, false},
		{"root parent", "1 (init) S 0", 0, false},
		{"no terminator", "123 boss S 42", 0, true},
		{"no parent", "123 (boss) S", 0, true},
		{"invalid parent", "123 (boss) S nope", 0, true},
		{"negative parent", "123 (boss) S -1", 0, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseParentPID(tc.stat)
			if (err != nil) != tc.wantErr {
				t.Fatalf("parse error = %v, want error %v", err, tc.wantErr)
			}
			if !tc.wantErr && got != tc.want {
				t.Fatalf("parent PID = %d, want %d", got, tc.want)
			}
		})
	}
}
