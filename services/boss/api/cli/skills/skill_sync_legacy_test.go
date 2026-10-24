package skills

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	libskillinstall "github.com/recurser/bossalib/contract/plugin/skillinstall"
)

// TestSkillDriftHistoryFollowsThePayloadMove pins BOS-1722's move of the
// checkout payload: a skill installed by a pre-move binary carries a blob that
// exists only in the history of the old source path. The drift walk does not
// follow renames, so without the legacy pathspec that healable "behind" install
// would classify as unrecoverable and fail every boss-plan / boss-build
// preflight after the move.
func TestSkillDriftHistoryFollowsThePayloadMove(t *testing.T) {
	g := newTestGroup()

	root := t.TempDir()
	runGit(t, root, "init", "--quiet", "-b", "main")
	runGit(t, root, "config", "maintenance.auto", "false")
	runGit(t, root, "config", "gc.auto", "0")

	rel := filepath.Join("skills", "boss", "SKILL.md")
	legacyFile := filepath.Join(root, filepath.FromSlash(legacySkillSourceRelPath), rel)
	commitFile := func(path, content, msg string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
		runGit(t, root, "add", "-A")
		runGit(t, root, "-c", "user.name=Test", "-c", "user.email=test@example.com", "commit", "--quiet", "-m", msg)
	}

	// v1 is what a pre-move binary installed; v2 is the content the move
	// carried, so v1 never appears at the new path.
	commitFile(legacyFile, "boss skill v1\n", "v1 at the legacy path")
	commitFile(legacyFile, "boss skill v2\n", "v2 at the legacy path")
	newDir := filepath.Join(root, filepath.FromSlash(libskillinstall.SourceRelPath))
	if err := os.MkdirAll(newDir, 0o755); err != nil {
		t.Fatal(err)
	}
	runGit(t, root, "mv", filepath.Join(filepath.FromSlash(legacySkillSourceRelPath), "skills"), filepath.Join(filepath.FromSlash(libskillinstall.SourceRelPath), "skills"))
	runGit(t, root, "-c", "user.name=Test", "-c", "user.email=test@example.com", "commit", "--quiet", "-m", "move the payload")
	commitFile(filepath.Join(newDir, rel), "boss skill v3\n", "v3 at the new path")

	sourcePath := filepath.Join(libskillinstall.SourceRelPath, rel)
	blobOf := func(content string) string {
		t.Helper()
		cmd := exec.Command("git", "-C", root, "hash-object", "--stdin")
		cmd.Stdin = strings.NewReader(content)
		out, err := cmd.Output()
		if err != nil {
			t.Fatalf("hash-object: %v", err)
		}
		return strings.TrimSpace(string(out))
	}

	if got := g.resolveSkillDriftBlobAncestry(root, blobOf("boss skill v1\n"), sourcePath); got != skillDriftBehind {
		t.Errorf("pre-move blob direction = %q, want %q: the walk must read the legacy path's history", got, skillDriftBehind)
	}
	if got := g.resolveSkillDriftBlobAncestry(root, blobOf("boss skill v3\n"), sourcePath); got != skillDriftBehind {
		t.Errorf("post-move blob direction = %q, want %q", got, skillDriftBehind)
	}
	if got := g.resolveSkillDriftBlobAncestry(root, blobOf("never committed\n"), sourcePath); got != skillDriftUnrecoverable {
		t.Errorf("uncommitted blob direction = %q, want %q: the legacy pathspec must not widen the walk past real history", got, skillDriftUnrecoverable)
	}
}
