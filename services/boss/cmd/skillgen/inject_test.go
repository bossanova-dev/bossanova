package skillgen

import (
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/recurser/bossalib/clidoc"
)

func TestGenerateProducesOneReferencePerGroup(t *testing.T) {
	bundle, err := Generate(newTestRoot())
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if len(bundle.References) != 2 {
		t.Fatalf("References = %d entries, want 2 (session, repo): %v", len(bundle.References), keysOf(bundle.References))
	}
	for _, key := range []string{"references/session.md", "references/repo.md"} {
		if _, ok := bundle.References[key]; !ok {
			t.Errorf("References missing %q; have %v", key, keysOf(bundle.References))
		}
	}
	rows := 0
	for _, line := range strings.Split(bundle.Index, "\n") {
		if strings.HasPrefix(line, "| `references/") {
			rows++
		}
	}
	if rows != len(bundle.References) {
		t.Errorf("index has %d reference rows, want %d\n---\n%s", rows, len(bundle.References), bundle.Index)
	}
}

func TestGenerateReferenceFilePreamble(t *testing.T) {
	bundle, err := Generate(newTestRoot())
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	ref := bundle.References["references/session.md"]
	lines := strings.Split(ref, "\n")
	if len(lines) < 3 {
		t.Fatalf("reference too short:\n%s", ref)
	}
	if !strings.HasPrefix(lines[0], "<!-- GENERATED") ||
		!strings.Contains(lines[0], "make gen-skill") ||
		!strings.Contains(lines[0], "../SKILL.md") {
		t.Errorf("line 1 must be the GENERATED comment pointing at the index, got %q", lines[0])
	}
	if lines[1] != "" {
		t.Errorf("line 2 must be blank, got %q", lines[1])
	}
	if lines[2] != "## Session Management" {
		t.Errorf("line 3 must be the group heading, got %q", lines[2])
	}
	if strings.Contains(ref, "\n# ") {
		t.Errorf("reference must not carry an H1\n---\n%s", ref)
	}
	if !strings.HasSuffix(ref, "\n") || strings.HasSuffix(ref, "\n\n") {
		t.Error("reference must end with exactly one trailing newline")
	}
}

func TestGenerateEmptyGroupsProduceNoReference(t *testing.T) {
	root := &cobra.Command{Use: "boss"}
	root.AddCommand(&cobra.Command{Use: "a", Short: "a", GroupID: "other", Run: func(*cobra.Command, []string) {}})
	root.AddCommand(&cobra.Command{Use: "b", Short: "b", GroupID: "other", Run: func(*cobra.Command, []string) {}})

	bundle, err := Generate(root)
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if len(bundle.References) != 1 {
		t.Fatalf("References = %v, want exactly references/other.md", keysOf(bundle.References))
	}
	if _, ok := bundle.References["references/other.md"]; !ok {
		t.Errorf("References missing references/other.md; have %v", keysOf(bundle.References))
	}
}

func TestGenerateIndexRowsCarryExactGroupOrderReadWhen(t *testing.T) {
	bundle, err := Generate(newTestRoot())
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	for _, id := range []string{"session", "repo"} {
		var want string
		for _, spec := range clidoc.GroupOrder {
			if spec.ID == id {
				want = spec.ReadWhen
			}
		}
		if want == "" {
			t.Fatalf("clidoc.GroupOrder has no ReadWhen for %q", id)
		}
		var got string
		for _, line := range strings.Split(bundle.Index, "\n") {
			if !strings.HasPrefix(line, "| `references/"+id+".md`") {
				continue
			}
			parts := strings.Split(strings.Trim(line, "|"), "|")
			if len(parts) != 2 {
				t.Fatalf("index row %q does not have 2 cells", line)
			}
			got = strings.TrimSpace(parts[1])
		}
		if got != want {
			t.Errorf("index row for %q carries %q, want the exact GroupOrder hint %q", id, got, want)
		}
	}
}

func keysOf(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

func TestInjectReplacesBetweenMarkers(t *testing.T) {
	skill := "# Title\n\nintro\n\n" + BeginMarker + "\nOLD\n" + EndMarker + "\n\n## Hand-authored\n\ntail\n"
	got, err := Inject(skill, "## New\n\nbody\n")
	if err != nil {
		t.Fatalf("Inject: %v", err)
	}
	if strings.Contains(got, "OLD") {
		t.Error("old generated content not removed")
	}
	if !strings.Contains(got, "## New") || !strings.Contains(got, "## Hand-authored") {
		t.Error("must replace generated region and keep hand-authored tail")
	}
	if !strings.Contains(got, BeginMarker) || !strings.Contains(got, EndMarker) {
		t.Error("markers must be preserved")
	}
	// Idempotent: injecting the same body again is a no-op.
	again, err := Inject(got, "## New\n\nbody\n")
	if err != nil {
		t.Fatalf("Inject (2nd): %v", err)
	}
	if again != got {
		t.Error("Inject is not idempotent")
	}
}

func TestInjectErrorsOnMissingMarkers(t *testing.T) {
	if _, err := Inject("no markers here", "x"); err == nil {
		t.Error("expected error when markers absent")
	}
	dup := BeginMarker + "\n" + BeginMarker + "\n" + EndMarker
	if _, err := Inject(dup, "x"); err == nil {
		t.Error("expected error on duplicate BEGIN marker")
	}
	dupEnd := BeginMarker + "\n" + EndMarker + "\n" + EndMarker
	if _, err := Inject(dupEnd, "x"); err == nil {
		t.Error("expected error on duplicate END marker")
	}
}

func TestInjectErrorsWhenEndPrecedesBegin(t *testing.T) {
	reversed := EndMarker + "\nstuff\n" + BeginMarker
	if _, err := Inject(reversed, "x"); err == nil {
		t.Error("expected error when END precedes BEGIN")
	}
}
