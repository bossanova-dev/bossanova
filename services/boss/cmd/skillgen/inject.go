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

	// referenceHeader is line 1 of every generated references/<id>.md. It is
	// followed by a blank line and then the group section verbatim — nothing
	// else — so a reference file is a pure two-line prefix over renderGroup.
	referenceHeader = "<!-- GENERATED from the boss CLI by `make gen-skill` — do not edit by hand. Index: ../SKILL.md -->"
)

// Bundle is one full render of the CLI reference: the routing index that lives
// between the SKILL.md markers, plus one reference file per non-empty group.
type Bundle struct {
	// Index is the body injected between the SKILL.md markers.
	Index string
	// References maps a core-relative path ("references/session.md") to that
	// file's complete content.
	References map[string]string
}

// Generate extracts root's command tree and renders it into a Bundle.
func Generate(root *cobra.Command) (Bundle, error) {
	groups, err := Extract(root)
	if err != nil {
		return Bundle{}, err
	}
	bundle := Bundle{
		Index:      renderIndex(groups, root.PersistentFlags()),
		References: make(map[string]string, len(groups)),
	}
	for _, g := range groups {
		bundle.References["references/"+g.ID+".md"] = referenceHeader + "\n\n" + renderGroup(g)
	}
	return bundle, nil
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
