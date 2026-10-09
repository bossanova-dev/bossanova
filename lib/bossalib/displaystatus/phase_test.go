package displaystatus

import (
	"strings"
	"testing"
)

func TestNormalizePhase(t *testing.T) {
	for _, input := range append(ConventionalPhases, "fixing ci", "  reviewing  ", "日本語") {
		got, err := NormalizePhase(input)
		if err != nil || got != strings.TrimSpace(input) {
			t.Errorf("NormalizePhase(%q) = %q, %v", input, got, err)
		}
	}
	for _, input := range []string{"", "   ", strings.Repeat("界", MaxPhaseRunes+1), "fix\nci", "fix\tci", "\x1b[31m", "fix\x7f", "fix\u2028ci", "fix\u2029ci"} {
		if _, err := NormalizePhase(input); err == nil {
			t.Errorf("NormalizePhase(%q) accepted invalid phase", input)
		}
	}
}
