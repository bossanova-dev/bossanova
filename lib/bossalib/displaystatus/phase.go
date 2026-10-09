package displaystatus

import (
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"
)

// MaxPhaseRunes bounds skill-reported phases displayed in terminal status cells.
const MaxPhaseRunes = 20

// ConventionalPhases documents the shared stage vocabulary; free text is allowed.
var ConventionalPhases = []string{"planning", "building", "reviewing", "verifying", "repairing", "releasing"}

// NormalizePhase trims a phase and validates that it is short and safe to render.
func NormalizePhase(raw string) (string, error) {
	phase := strings.TrimSpace(raw)
	if phase == "" {
		return "", fmt.Errorf("phase must not be empty")
	}
	if !utf8.ValidString(phase) {
		return "", fmt.Errorf("phase must contain valid printable text")
	}
	if utf8.RuneCountInString(phase) > MaxPhaseRunes {
		return "", fmt.Errorf("phase must be at most %d characters", MaxPhaseRunes)
	}
	for _, r := range phase {
		if !unicode.IsPrint(r) {
			return "", fmt.Errorf("phase must be a single line of printable characters")
		}
	}
	return phase, nil
}
