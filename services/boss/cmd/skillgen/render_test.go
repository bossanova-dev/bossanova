package skillgen

import (
	"strings"
	"testing"

	"github.com/spf13/pflag"

	"github.com/recurser/bossalib/clidoc"
)

func TestRenderIsDeterministicAndUnwrapped(t *testing.T) {
	groups := []clidoc.GroupDoc{{
		ID:    "session",
		Title: "Session Management",
		Commands: []clidoc.CommandDoc{{
			Path:      "boss ls",
			UsageLine: "boss ls [flags]",
			Synopsis:  "List sessions (non-interactive).",
			Long:      "An extra AGENT column appears only when relevant.",
			Flags: []clidoc.FlagDoc{
				{Name: "archived", Usage: "Include archived sessions", Default: ""},
				{Name: "repo", Usage: "Filter by repo ID"},
			},
			Examples: []clidoc.Example{{Command: "boss ls"}},
		}},
	}}
	gf := pflag.NewFlagSet("boss", pflag.ContinueOnError)
	gf.String("remote", "", "Connect to an orchestrator URL instead of the local daemon.")

	out := renderGlobalFlags(gf) + renderGroup(groups[0])

	for _, want := range []string{
		"## Global Flags",
		"### `--remote`",
		"## Session Management",
		"### `boss ls [flags]`",
		"- `--archived` — Include archived sessions",
		"- `--repo` — Filter by repo ID",
		"An extra AGENT column appears only when relevant.",
		"```bash\nboss ls\n```",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("render missing %q\n---\n%s", want, out)
		}
	}
	if !strings.HasSuffix(out, "\n") || strings.HasSuffix(out, "\n\n") {
		t.Errorf("render must end with exactly one trailing newline")
	}
	// Determinism: identical inputs render byte-identical.
	if renderGlobalFlags(gf)+renderGroup(groups[0]) != out {
		t.Error("render is not deterministic")
	}
}

func TestRenderFlagDecorations(t *testing.T) {
	groups := []clidoc.GroupDoc{{
		ID:    "trash",
		Title: "Trash Management",
		Commands: []clidoc.CommandDoc{{
			Path:      "boss trash delete",
			UsageLine: "boss trash delete [flags]",
			Synopsis:  "Delete an archived session",
			Flags: []clidoc.FlagDoc{
				{Name: "limit", Shorthand: "", Usage: "Number of snapshots", Default: "5"},
				{Name: "must", Usage: "Required flag", Required: true},
				{Name: "old", Usage: "Old flag", Deprecated: true, DeprecationMsg: "use --new"},
				{Name: "yes", Shorthand: "y", Usage: "Skip confirmation prompt"},
			},
		}},
	}}
	out := renderGroup(groups[0])

	for _, want := range []string{
		"- `--limit` — Number of snapshots (default: 5)",
		"- `--must` — Required flag (required)",
		"- `--old` — Old flag (deprecated: use --new)",
		"- `--yes`, `-y` — Skip confirmation prompt",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("render missing %q\n---\n%s", want, out)
		}
	}
}

func TestRenderAliasAndExampleExplanation(t *testing.T) {
	groups := []clidoc.GroupDoc{{
		ID:    "plugins",
		Title: "Plugins",
		Commands: []clidoc.CommandDoc{{
			Path:      "boss plugin list",
			UsageLine: "boss plugin list",
			Synopsis:  "List plugins",
			Aliases:   []string{"ls"}, // cobra alias is the bare leaf name
			Examples: []clidoc.Example{
				{Command: "boss plugin list", Explanation: "long form"},
				{Command: "boss plugin ls"},
			},
		}},
	}}
	out := renderGroup(groups[0])

	// The alias renders under the parent path, not bare under "boss".
	if !strings.Contains(out, "Alias: `boss plugin ls`") {
		t.Errorf("render missing alias line\n---\n%s", out)
	}
	if strings.Contains(out, "Alias: `boss ls`") {
		t.Errorf("alias must not be rendered bare under boss\n---\n%s", out)
	}
	if !strings.Contains(out, "```bash\n# long form\nboss plugin list\nboss plugin ls\n```") {
		t.Errorf("render missing example block with explanation\n---\n%s", out)
	}
}

func indexTestGroups() []clidoc.GroupDoc {
	return []clidoc.GroupDoc{
		{
			ID:       "session",
			Title:    "Session Management",
			ReadWhen: "Creating, listing, attaching to, merging or archiving a session",
			Commands: []clidoc.CommandDoc{{Path: "boss ls", UsageLine: "boss ls", Synopsis: "List sessions"}},
		},
		{
			ID:       "mcp",
			Title:    "MCP Server",
			ReadWhen: "Running or configuring the MCP server",
			Commands: []clidoc.CommandDoc{{Path: "boss mcp", UsageLine: "boss mcp", Synopsis: "Run the MCP server"}},
		},
	}
}

// tableLines returns the contiguous markdown-table lines of s, in order.
func tableLines(s string) []string {
	var out []string
	for _, line := range strings.Split(s, "\n") {
		if strings.HasPrefix(line, "|") {
			out = append(out, line)
		}
	}
	return out
}

// pipeOffsets returns the rune offsets of every "|" in line. Prettier aligns
// markdown tables on rune width, so identical offsets across rows is exactly
// the property that makes the emitted table already prettier-settled.
func pipeOffsets(line string) []int {
	var out []int
	for i, r := range []rune(line) {
		if r == '|' {
			out = append(out, i)
		}
	}
	return out
}

func offsetsEqual(a, b []int) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestRenderIndexTableIsColumnAligned(t *testing.T) {
	gf := pflag.NewFlagSet("boss", pflag.ContinueOnError)
	gf.String("remote", "", "Connect to an orchestrator URL instead of the local daemon.")

	out := renderIndex(indexTestGroups(), gf)

	rows := tableLines(out)
	if len(rows) != 4 { // header + separator + one row per group
		t.Fatalf("expected 4 table lines, got %d\n---\n%s", len(rows), out)
	}
	want := pipeOffsets(rows[0])
	if len(want) != 3 {
		t.Fatalf("header row must have 3 pipes, got %d: %q", len(want), rows[0])
	}
	for i, row := range rows[1:] {
		if got := pipeOffsets(row); !offsetsEqual(got, want) {
			t.Errorf("table row %d pipe offsets %v != header %v\nrow: %q\nhdr: %q", i+1, got, want, row, rows[0])
		}
	}
}

func TestRenderIndexRoutesToBareCoreRelativeReferences(t *testing.T) {
	gf := pflag.NewFlagSet("boss", pflag.ContinueOnError)
	gf.String("remote", "", "Connect to an orchestrator URL instead of the local daemon.")

	out := renderIndex(indexTestGroups(), gf)

	for _, want := range []string{
		"## Global Flags",
		"### `--remote`",
		"## Command Groups",
		"`references/session.md`",
		"`references/mcp.md`",
		"| Reference",
		"Read it when…",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("index missing %q\n---\n%s", want, out)
		}
	}
	// crossCoreReferencePattern in skills_manifest_test.go makes a core-prefixed
	// spelling illegal — the index must never emit one.
	if strings.Contains(out, "boss/references/") {
		t.Errorf("index must spell references bare and core-relative\n---\n%s", out)
	}
	if !strings.HasSuffix(out, "\n") || strings.HasSuffix(out, "\n\n") {
		t.Errorf("index must end with exactly one trailing newline")
	}
}

func TestRenderIndexRowsCarryExactReadWhen(t *testing.T) {
	gf := pflag.NewFlagSet("boss", pflag.ContinueOnError)
	groups := indexTestGroups()
	out := renderIndex(groups, gf)

	cells := map[string]string{}
	for _, row := range tableLines(out) {
		parts := strings.Split(strings.Trim(row, "|"), "|")
		if len(parts) != 2 {
			t.Fatalf("table row %q does not have 2 cells", row)
		}
		cells[strings.TrimSpace(parts[0])] = strings.TrimSpace(parts[1])
	}
	for _, g := range groups {
		key := "`references/" + g.ID + ".md`"
		got, ok := cells[key]
		if !ok {
			t.Fatalf("index has no row for %s\n---\n%s", key, out)
		}
		if got != g.ReadWhen {
			t.Errorf("row %s hint = %q, want %q", key, got, g.ReadWhen)
		}
	}
}
