package agentruntime

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestStripXMLTags(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"plain text", "hello world", "hello world"},
		{"empty", "", ""},
		{"single tag pair", "<thinking>plan</thinking>", "plan"},
		{"surrounding whitespace trimmed", "  <tag>x</tag>  ", "x"},
		{"self-closing tag", "before<br/>after", "beforeafter"},
		{"lone open tag", "<command-name>", ""},
		{"tag with attributes", `<a href="x">link</a>`, "link"},
		{"less-than not a tag", "a < b and c > d", "a < b and c > d"},
		{"unclosed angle bracket kept", "<unclosed", "<unclosed"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := StripXMLTags(tt.in); got != tt.want {
				t.Errorf("StripXMLTags(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

func TestFirstLine(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"single line", "just one line", "just one line"},
		{"multi line keeps first", "first\nsecond\nthird", "first"},
		{"empty", "", ""},
		{"leading and trailing whitespace", "  padded  ", "padded"},
		{"leading newlines stripped", "\n\nfoo\nbar", "foo"},
		{"trailing newline", "solo\n", "solo"},
		{"whitespace before newline", "  line one  \n line two", "line one"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := FirstLine(tt.in); got != tt.want {
				t.Errorf("FirstLine(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

func TestTruncate(t *testing.T) {
	atLimit := strings.Repeat("a", MaxSummaryLen)
	overLimit := strings.Repeat("b", MaxSummaryLen+5)
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"short string unchanged", "short", "short"},
		{"empty unchanged", "", ""},
		{"exactly at limit unchanged", atLimit, atLimit},
		{"over limit truncated with Ellipsis", overLimit, strings.Repeat("b", MaxSummaryLen-3) + Ellipsis},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := TruncateSummary(tt.in)
			if got != tt.want {
				t.Errorf("TruncateSummary(%q) = %q, want %q", tt.in, got, tt.want)
			}
			if len(got) > MaxSummaryLen {
				t.Errorf("TruncateSummary output length %d exceeds MaxSummaryLen %d", len(got), MaxSummaryLen)
			}
		})
	}
}

func TestTruncate_CutFallsInsideMultiByteRune(t *testing.T) {
	// 76 ASCII bytes then 3-byte runes, so byte MaxSummaryLen-3 (77) is a
	// continuation byte in the middle of the first multi-byte rune.
	in := strings.Repeat("a", 76) + strings.Repeat("あ", 3)
	if len(in) <= MaxSummaryLen {
		t.Fatalf("fixture is %d bytes, need more than MaxSummaryLen (%d) to TruncateSummary", len(in), MaxSummaryLen)
	}
	if utf8.RuneStart(in[MaxSummaryLen-len(Ellipsis)]) {
		t.Fatalf("fixture does not exercise the bug: byte %d is already a rune boundary", MaxSummaryLen-len(Ellipsis))
	}

	got := TruncateSummary(in)

	if !utf8.ValidString(got) {
		t.Errorf("TruncateSummary(...) = %q, which is not valid UTF-8 — the cut split a rune", got)
	}
	if len(got) > MaxSummaryLen {
		t.Errorf("TruncateSummary(...) = %q, %d bytes, want at most MaxSummaryLen (%d)", got, len(got), MaxSummaryLen)
	}
	if !strings.HasSuffix(got, Ellipsis) {
		t.Errorf("TruncateSummary(...) = %q, want it to end in the Ellipsis marker %q", got, Ellipsis)
	}
	if want := strings.Repeat("a", 76) + Ellipsis; got != want {
		t.Errorf("TruncateSummary(...) = %q, want %q", got, want)
	}
}

func TestNonEmptyFile(t *testing.T) {
	dir := t.TempDir()
	for _, tt := range []struct {
		name, contents string
		want           bool
	}{
		{"empty", "", false}, {"nonempty", "text", true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join(dir, tt.name)
			if err := os.WriteFile(path, []byte(tt.contents), 0o600); err != nil {
				t.Fatal(err)
			}
			if got := NonEmptyFile(path); got != tt.want {
				t.Errorf("NonEmptyFile(%q) = %v, want %v", path, got, tt.want)
			}
		})
	}
	t.Run("missing", func(t *testing.T) {
		if NonEmptyFile(filepath.Join(dir, "missing")) {
			t.Error("missing file reported nonempty")
		}
	})
	t.Run("directory", func(t *testing.T) {
		if NonEmptyFile(dir) {
			t.Error("directory reported nonempty")
		}
	})
}
