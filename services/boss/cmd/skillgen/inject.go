package skillgen

import (
	"fmt"
	"strings"

	"github.com/spf13/cobra"
)

const (
	// BeginMarker and EndMarker delimit the generated command-reference region
	// of the canonical SKILL.md. Everything between them is owned by
	// `boss gen-skill`; everything outside is hand-authored.
	BeginMarker = "<!-- BEGIN GENERATED: boss command reference — run `make gen-skill` -->"
	EndMarker   = "<!-- END GENERATED -->"
)

// GenerateBody extracts and renders the full reference body for root.
func GenerateBody(root *cobra.Command) (string, error) {
	groups, err := Extract(root)
	if err != nil {
		return "", err
	}
	return Render(groups, root.PersistentFlags()), nil
}

// Inject replaces the region between the markers in skill with body. The
// markers themselves are preserved. Errors on missing/duplicate/misordered
// markers so corruption is loud, never silent.
func Inject(skill, body string) (string, error) {
	if n := strings.Count(skill, BeginMarker); n != 1 {
		return "", fmt.Errorf("expected exactly 1 %q, found %d", BeginMarker, n)
	}
	if n := strings.Count(skill, EndMarker); n != 1 {
		return "", fmt.Errorf("expected exactly 1 %q, found %d", EndMarker, n)
	}
	begin := strings.Index(skill, BeginMarker)
	end := strings.Index(skill, EndMarker)
	if end < begin {
		return "", fmt.Errorf("END marker precedes BEGIN marker")
	}
	head := skill[:begin+len(BeginMarker)]
	tail := skill[end:]
	return head + "\n\n" + strings.TrimRight(body, "\n") + "\n\n" + tail, nil
}
