package views

import "testing"

func TestOSCNotificationSequence(t *testing.T) {
	req := notifyRequest{Title: "Bossanova", Body: "MV Plans needs your input", Sound: true}

	tests := []struct {
		name   string
		env    notifyEnv
		req    notifyRequest
		want   string
		wantOK bool
	}{
		{
			name:   "ghostty direct uses OSC 777 title+body",
			env:    notifyEnv{ghosttyResource: "/Applications/Ghostty.app/.../ghostty"},
			req:    req,
			want:   "\x1b]777;notify;Bossanova;MV Plans needs your input\a",
			wantOK: true,
		},
		{
			name:   "ghostty via TERM uses OSC 777",
			env:    notifyEnv{term: "xterm-ghostty"},
			req:    req,
			want:   "\x1b]777;notify;Bossanova;MV Plans needs your input\a",
			wantOK: true,
		},
		{
			name:   "ghostty under tmux is wrapped in passthrough",
			env:    notifyEnv{ghosttyResource: "/x", tmux: "/private/tmp/tmux-501/default,123,0"},
			req:    req,
			want:   "\x1bPtmux;\x1b\x1b]777;notify;Bossanova;MV Plans needs your input\a\x1b\\",
			wantOK: true,
		},
		{
			name:   "wezterm uses OSC 777",
			env:    notifyEnv{weztermPane: "0"},
			req:    req,
			want:   "\x1b]777;notify;Bossanova;MV Plans needs your input\a",
			wantOK: true,
		},
		{
			name:   "iterm2 uses OSC 9 with title folded into body",
			env:    notifyEnv{itermSession: "w0t0p0:UUID"},
			req:    req,
			want:   "\x1b]9;Bossanova: MV Plans needs your input\a",
			wantOK: true,
		},
		{
			name:   "unknown terminal is unsupported",
			env:    notifyEnv{term: "xterm-256color", termProgram: "Apple_Terminal"},
			req:    req,
			wantOK: false,
		},
		{
			name:   "empty request is unsupported",
			env:    notifyEnv{ghosttyResource: "/x"},
			req:    notifyRequest{},
			wantOK: false,
		},
		{
			name:   "field separators and escapes in body are sanitized",
			env:    notifyEnv{ghosttyResource: "/x"},
			req:    notifyRequest{Title: "Bossanova", Body: "a;b\x1bc\nd"},
			want:   "\x1b]777;notify;Bossanova;a b c d\a",
			wantOK: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := oscNotificationSequence(tt.env, tt.req)
			if ok != tt.wantOK {
				t.Fatalf("ok = %v, want %v (got %q)", ok, tt.wantOK, got)
			}
			if !tt.wantOK {
				return
			}
			if got != tt.want {
				t.Errorf("sequence =\n  %q\nwant\n  %q", got, tt.want)
			}
		})
	}
}

func TestRunNotifyPrefersOSCWhenCapable(t *testing.T) {
	origEnv, origWrite := notifyEnvSource, writeTerminalSequence
	t.Cleanup(func() { notifyEnvSource, writeTerminalSequence = origEnv, origWrite })

	// An OSC-capable terminal writes the sequence to the tty and never shells
	// out to the OS notifier (which would fire a real osascript banner here).
	notifyEnvSource = func() notifyEnv { return notifyEnv{ghosttyResource: "/x"} }
	var wrote string
	writeTerminalSequence = func(seq string) error { wrote = seq; return nil }

	if err := runNotify(notifyRequest{Title: "Bossanova", Body: "needs input"}); err != nil {
		t.Fatalf("runNotify() error = %v", err)
	}
	want := "\x1b]777;notify;Bossanova;needs input\a"
	if wrote != want {
		t.Errorf("wrote %q, want %q", wrote, want)
	}
}

// An unsupported terminal must not take the OSC path — terminalNotificationSequence
// reports false so runNotify falls through to the OS-level notifier. Asserted
// directly to avoid actually shelling out to osascript in a unit test.
func TestUnsupportedTerminalSkipsOSC(t *testing.T) {
	origEnv := notifyEnvSource
	t.Cleanup(func() { notifyEnvSource = origEnv })
	notifyEnvSource = func() notifyEnv { return notifyEnv{term: "xterm-256color", termProgram: "Apple_Terminal"} }

	if seq, ok := terminalNotificationSequence(notifyRequest{Title: "Bossanova", Body: "needs input"}); ok {
		t.Errorf("terminalNotificationSequence ok = true (%q), want false for an unsupported terminal", seq)
	}
}
