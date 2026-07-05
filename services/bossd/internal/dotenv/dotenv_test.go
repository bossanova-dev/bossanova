package dotenv

import (
	"maps"
	"os"
	"path/filepath"
	"testing"
)

// writeEnvFile creates dir/.env with the given content and returns dir.
func writeEnvFile(t *testing.T, content string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, ".env"), []byte(content), 0o600); err != nil {
		t.Fatalf("write .env: %v", err)
	}
	return dir
}

func TestLoadMissingFileReturnsNil(t *testing.T) {
	if got := Load(t.TempDir()); got != nil {
		t.Fatalf("Load on dir without .env = %v, want nil", got)
	}
}

func TestLoadEmptyAndCommentOnlyFileReturnsNil(t *testing.T) {
	dir := writeEnvFile(t, "# only a comment\n\n   \n# another\n")
	if got := Load(dir); got != nil {
		t.Fatalf("Load on comment-only .env = %v, want nil", got)
	}
}

func TestLoadParsesRealWorldShapes(t *testing.T) {
	dir := writeEnvFile(t, `# Linear
LINEAR_API_KEY=lin_api_abc123

export EXPORTED=yes
DOUBLE_QUOTED="hello world"
SINGLE_QUOTED='single value'
SPACED_KEY = padded
EMPTY=
HASH_IN_VALUE=se#cret
CRLF_VALUE=trimmed`+"\r\n")
	want := map[string]string{
		"LINEAR_API_KEY": "lin_api_abc123",
		"EXPORTED":       "yes",
		"DOUBLE_QUOTED":  "hello world",
		"SINGLE_QUOTED":  "single value",
		"SPACED_KEY":     "padded",
		"EMPTY":          "",
		"HASH_IN_VALUE":  "se#cret",
		"CRLF_VALUE":     "trimmed",
	}
	got := Load(dir)
	if !maps.Equal(got, want) {
		t.Fatalf("Load = %v, want %v", got, want)
	}
}

func TestLoadSkipsMalformedLines(t *testing.T) {
	dir := writeEnvFile(t, `no_equals_sign_here
=value_without_key
1BAD=starts with digit
BAD KEY=space in key
bad-key=hyphen
GOOD=kept
"QUOTED_KEY"=skipped
`)
	want := map[string]string{"GOOD": "kept"}
	got := Load(dir)
	if !maps.Equal(got, want) {
		t.Fatalf("Load = %v, want %v", got, want)
	}
}

func TestLoadKeepsUnbalancedQuotes(t *testing.T) {
	dir := writeEnvFile(t, `LEADING="unclosed
MIXED="double'
SHORT="
`)
	want := map[string]string{
		"LEADING": `"unclosed`,
		"MIXED":   `"double'`,
		"SHORT":   `"`,
	}
	got := Load(dir)
	if !maps.Equal(got, want) {
		t.Fatalf("Load = %v, want %v", got, want)
	}
}

func TestOverlayBaseWinsOnConflict(t *testing.T) {
	dir := writeEnvFile(t, "BOSS_SETTINGS_PATH=/from/dotenv\nLINEAR_API_KEY=lin_api_abc123\n")
	base := map[string]string{"BOSS_SETTINGS_PATH": "/managed/settings.json"}
	got := Overlay(base, dir)
	want := map[string]string{
		"BOSS_SETTINGS_PATH": "/managed/settings.json",
		"LINEAR_API_KEY":     "lin_api_abc123",
	}
	if !maps.Equal(got, want) {
		t.Fatalf("Overlay = %v, want %v", got, want)
	}
	// The input map must not be mutated.
	if len(base) != 1 {
		t.Fatalf("Overlay mutated base: %v", base)
	}
}

func TestOverlayWithoutDotenvReturnsBaseUnchanged(t *testing.T) {
	base := map[string]string{"BOSS_AGENT": "claude"}
	if got := Overlay(base, t.TempDir()); !maps.Equal(got, base) {
		t.Fatalf("Overlay without .env = %v, want %v", got, base)
	}
}

func TestOverlayNilBaseReturnsDotenvOnly(t *testing.T) {
	dir := writeEnvFile(t, "LINEAR_API_KEY=lin_api_abc123\n")
	got := Overlay(nil, dir)
	want := map[string]string{"LINEAR_API_KEY": "lin_api_abc123"}
	if !maps.Equal(got, want) {
		t.Fatalf("Overlay(nil) = %v, want %v", got, want)
	}
}

func TestOverlayNilBaseNoDotenvReturnsNil(t *testing.T) {
	if got := Overlay(nil, t.TempDir()); got != nil {
		t.Fatalf("Overlay(nil, empty dir) = %v, want nil", got)
	}
}
