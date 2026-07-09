package session

import "testing"

func TestParseBossControlCommand(t *testing.T) {
	tests := []struct {
		name        string
		message     string
		wantOK      bool
		wantAccount string
		wantForce   bool
	}{
		// POSITIVE
		{name: "boss switch bare", message: "/boss switch", wantOK: true},
		{name: "boss switch account", message: "/boss switch alpha", wantOK: true, wantAccount: "alpha"},
		{name: "boss switch account force", message: "/boss switch alpha --force", wantOK: true, wantAccount: "alpha", wantForce: true},
		{name: "boss switch force only", message: "/boss switch --force", wantOK: true, wantForce: true},
		{name: "boss switch force before account", message: "/boss switch --force alpha", wantOK: true, wantAccount: "alpha", wantForce: true},
		{name: "switch bare", message: "/switch", wantOK: true},
		{name: "switch account", message: "/switch alpha", wantOK: true, wantAccount: "alpha"},
		{name: "switch account force", message: "/switch alpha --force", wantOK: true, wantAccount: "alpha", wantForce: true},
		{name: "boss switch surrounding whitespace", message: "  /boss switch  ", wantOK: true},
		{name: "switch leading tab", message: "\t/switch alpha", wantOK: true, wantAccount: "alpha"},

		// NEGATIVE
		{name: "prose containing boss switch", message: "please /boss switch now", wantOK: false},
		{name: "multiline first line boss switch", message: "/boss switch\nmore", wantOK: false},
		{name: "multiline carriage return", message: "/switch\r\nmore", wantOK: false},
		{name: "bossswitch no space", message: "/bossswitch", wantOK: false},
		{name: "boss alone", message: "/boss", wantOK: false},
		{name: "boss switcheroo", message: "/boss switcheroo", wantOK: false},
		{name: "switchfoo", message: "/switchfoo", wantOK: false},
		{name: "hello world", message: "hello world", wantOK: false},
		{name: "model command", message: "/model", wantOK: false},
		{name: "empty string", message: "", wantOK: false},
		{name: "boss switch second positional", message: "/boss switch a b", wantOK: false},
		{name: "switch second positional", message: "/switch a b", wantOK: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := ParseBossControlCommand(tt.message)
			if ok != tt.wantOK {
				t.Fatalf("ParseBossControlCommand(%q) ok = %v, want %v", tt.message, ok, tt.wantOK)
			}
			if !tt.wantOK {
				if got != nil {
					t.Fatalf("ParseBossControlCommand(%q) = %+v, want nil pointer on rejection", tt.message, got)
				}
				return
			}
			if got == nil {
				t.Fatalf("ParseBossControlCommand(%q) returned nil pointer with ok=true", tt.message)
			}
			if got.Account != tt.wantAccount {
				t.Errorf("ParseBossControlCommand(%q).Account = %q, want %q", tt.message, got.Account, tt.wantAccount)
			}
			if got.Force != tt.wantForce {
				t.Errorf("ParseBossControlCommand(%q).Force = %v, want %v", tt.message, got.Force, tt.wantForce)
			}
		})
	}
}
