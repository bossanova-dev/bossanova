//go:build e2e

package main

import (
	"testing"

	"github.com/recurser/boss/internal/views"
)

// TestResolveE2EHostAttachDestination covers the BOS-714 fixture seed: only a
// genuinely non-empty, non-falsey BOSS_HOST_E2E_ATTACH_DESTINATION puts the TUI
// into a remote context, and the value is used as the ssh destination verbatim.
func TestResolveE2EHostAttachDestination(t *testing.T) {
	tests := []struct {
		name  string
		value string
		want  string
	}{
		{name: "unset means no seed"},
		{name: "a destination is used verbatim", value: "deploy@build-box", want: "deploy@build-box"},
		{name: "surrounding space is trimmed", value: "  deploy@build-box  ", want: "deploy@build-box"},
		// An operator who exports the var as "0" plainly means off; putting their
		// run into a remote context instead would route attach over ssh to a host
		// they never named.
		{name: "zero means off", value: "0"},
		{name: "false with case and space means off", value: "  FALSE "},
		{name: "off means off", value: "off"},
		{name: "no means off", value: "no"},
		// The value becomes argv for ssh, and scenario files are agent-authored
		// while ProofEnvWhitelist validates keys only. `-oProxyCommand=…` is read
		// by ssh as an option and runs an arbitrary command on the harness, so an
		// option-shaped value must not survive to become a destination.
		{name: "an option-shaped value is refused", value: "-oProxyCommand=touch /tmp/pwned"},
		{name: "a lone dash flag is refused", value: "-F/tmp/evil_config"},
		{name: "a value carrying a second argument is refused", value: "host -oProxyCommand=id"},
		{name: "an embedded newline is refused", value: "host\nnot-a-host"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("BOSS_HOST_E2E_ATTACH_DESTINATION", tt.value)
			if got := resolveE2EHostAttachDestination(); got != tt.want {
				t.Fatalf("resolveE2EHostAttachDestination() = %q, want %q", got, tt.want)
			}
		})
	}
}

// TestApplyE2EHostAttachSeedOnlySetsTheContextWhenAsked pins both directions of
// the seed against the switch it actually flips. The "unset" half matters most:
// a seed that set an empty destination would still be harmless, but one that ran
// unconditionally would silently make every e2e run behave like --host.
func TestApplyE2EHostAttachSeedOnlySetsTheContextWhenAsked(t *testing.T) {
	// The seed writes a process-global switch; restore it so a sibling test in
	// this package never inherits a remote context.
	t.Cleanup(func() { views.SetHostDestination("") })

	t.Setenv("BOSS_HOST_E2E_ATTACH_DESTINATION", "")
	if got := applyE2EHostAttachSeed(); got != "" {
		t.Fatalf("applyE2EHostAttachSeed() = %q with no seed, want no destination applied", got)
	}

	t.Setenv("BOSS_HOST_E2E_ATTACH_DESTINATION", "deploy@build-box")
	if got := applyE2EHostAttachSeed(); got != "deploy@build-box" {
		t.Fatalf("applyE2EHostAttachSeed() = %q, want the seeded destination applied", got)
	}
}
