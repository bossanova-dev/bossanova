package config

import "testing"

func TestResolveCloudControl(t *testing.T) {
	for _, raw := range []string{"", "read-only", "full", " full ", "FULL", "yes", "private", " private ", "PRIVATE", "privat"} {
		t.Run(raw, func(t *testing.T) {
			want := CloudControlReadOnly
			if raw == "full" || raw == " full " {
				want = CloudControlFull
			}
			if raw == "private" || raw == " private " {
				want = CloudControlMode("private")
			}
			if got := ResolveCloudControl(raw); got != want {
				t.Fatalf("mode = %v, want %v", got, want)
			}
		})
	}
}
