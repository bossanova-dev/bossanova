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

	out := Render(groups, gf)

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
	if !strings.HasSuffix(out, "\n") || strings.HasSuffix(out, "\n\n\n") {
		t.Errorf("render must end with exactly one trailing newline")
	}
	// Determinism: identical inputs render byte-identical.
	if Render(groups, gf) != out {
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
	gf := pflag.NewFlagSet("boss", pflag.ContinueOnError)
	out := Render(groups, gf)

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
	gf := pflag.NewFlagSet("boss", pflag.ContinueOnError)
	out := Render(groups, gf)

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
