package skills

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"

	libskillinstall "github.com/recurser/bossalib/contract/plugin/skillinstall"
)

// TestSkillDriftHistoryFollowsThePayloadMove checks recoverability across roots.
func TestSkillDriftHistoryFollowsThePayloadMove(t *testing.T) {
	g := newTestGroup()

	root := t.TempDir()
	runGit(t, root, "init", "--quiet", "-b", "main")
	runGit(t, root, "config", "maintenance.auto", "false")
	runGit(t, root, "config", "gc.auto", "0")
	runGit(t, root, "config", "commit.gpgsign", "false")

	rel := filepath.Join("skills", "boss", "SKILL.md")
	legacyFile := filepath.Join(root, filepath.FromSlash("historical/payload"), rel)
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
	runGit(t, root, "mv", filepath.Join(filepath.FromSlash("historical/payload"), "skills"), filepath.Join(filepath.FromSlash(libskillinstall.SourceRelPath), "skills"))
	runGit(t, root, "-c", "user.name=Test", "-c", "user.email=test@example.com", "commit", "--quiet", "-m", "move the payload")
	commitFile(filepath.Join(newDir, rel), "boss skill v3\n", "v3 at the new path")

	history := &skillDriftHistory{}
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

	if got := g.resolveSkillDriftBlobAncestry(root, blobOf("boss skill v1\n"), history); got != skillDriftBehind {
		t.Errorf("pre-move blob direction = %q, want %q: the walk must read the legacy path's history", got, skillDriftBehind)
	}
	if got := g.resolveSkillDriftBlobAncestry(root, blobOf("boss skill v3\n"), history); got != skillDriftBehind {
		t.Errorf("post-move blob direction = %q, want %q", got, skillDriftBehind)
	}
	if got := g.resolveSkillDriftBlobAncestry(root, blobOf("never committed\n"), history); got != skillDriftUnrecoverable {
		t.Errorf("uncommitted blob direction = %q, want %q: the legacy pathspec must not widen the walk past real history", got, skillDriftUnrecoverable)
	}
	// Origin-only bytes are recoverable even at a different filename in toolbox sources.
	runGit(t, root, "update-ref", "refs/remotes/origin/main", "HEAD")
	runGit(t, root, "symbolic-ref", "refs/remotes/origin/HEAD", "refs/remotes/origin/main")
	commitFile(filepath.Join(root, "skills-toolbox", "different.mjs"), "origin-only\n", "future toolbox")
	runGit(t, root, "update-ref", "refs/remotes/origin/main", "HEAD")
	runGit(t, root, "checkout", "--detach", "HEAD~1")
	if got := g.resolveSkillDriftBlobAncestry(root, blobOf("origin-only\n"), &skillDriftHistory{}); got != skillDriftAhead {
		t.Fatalf("origin-only direction = %q", got)
	}
	commitFile(filepath.Join(root, "skills-toolbox", "orphan.mjs"), "unreachable\n", "unreachable branch")
	runGit(t, root, "checkout", "--detach", "HEAD~1")
	if got := g.resolveSkillDriftBlobAncestry(root, blobOf("unreachable\n"), &skillDriftHistory{}); got != skillDriftUnrecoverable {
		t.Fatalf("unreachable direction = %q", got)
	}

}

func TestSkillDriftHistory_BossSkills_AC6_MovedToolboxHeal(t *testing.T) {
	for _, stray := range []bool{true, false} {
		t.Run(fmt.Sprint(stray), func(t *testing.T) {
			g := newTestGroup()
			home := g.setupSkillStartupTest(t)
			root := t.TempDir()
			initial := gateSkillFS()
			initial["skills/boss/toolbox/x.mjs"] = &fstest.MapFile{Data: []byte("old root\n")}
			src := writeSkillSources(t, root, initial)
			runGit(t, root, "config", "commit.gpgsign", "false")
			runGit(t, root, "add", ".")
			oldRoot := "historical/old"
			if err := os.MkdirAll(filepath.Join(root, oldRoot), 0o755); err != nil {
				t.Fatal(err)
			}
			runGit(t, root, "mv", filepath.Join(libskillinstall.SourceRelPath, "skills"), filepath.Join(oldRoot, "skills"))
			commitCheckoutAsOriginHead(t, root)
			middle := "historical/middle"
			if err := os.MkdirAll(filepath.Join(root, middle), 0o755); err != nil {
				t.Fatal(err)
			}
			runGit(t, root, "mv", filepath.Join(oldRoot, "skills"), filepath.Join(middle, "skills"))
			middleFile := filepath.Join(root, middle, "skills/boss/toolbox/x.mjs")
			installed := []byte("middle root only\n")
			if err := os.WriteFile(middleFile, installed, 0o644); err != nil {
				t.Fatal(err)
			}
			commitCheckoutAsOriginHead(t, root)
			runGit(t, root, "mv", filepath.Join(middle, "skills"), filepath.Join(libskillinstall.SourceRelPath, "skills"))
			currentRel := "boss/toolbox/x.mjs"
			if stray {
				if err := os.MkdirAll(filepath.Join(src, "skills/boss/toolbox/sub"), 0o755); err != nil {
					t.Fatal(err)
				}
				runGit(t, root, "mv", filepath.Join(libskillinstall.SourceRelPath, "skills", currentRel), filepath.Join(libskillinstall.SourceRelPath, "skills/boss/toolbox/sub/x.mjs"))
				currentRel = "boss/toolbox/sub/x.mjs"
			}
			if err := os.WriteFile(filepath.Join(src, "skills", currentRel), []byte("current root\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			commitCheckoutAsOriginHead(t, root)
			dir := filepath.Join(home, ".claude/skills")
			current := gateSkillFS()
			current["skills/"+currentRel] = &fstest.MapFile{Data: []byte("current root\n")}
			if err := libskillinstall.Extract(dir, current); err != nil {
				t.Fatal(err)
			}
			stale := filepath.Join(dir, libskillinstall.Namespace, "boss/toolbox/x.mjs")
			if err := os.WriteFile(stale, installed, 0o644); err != nil {
				t.Fatal(err)
			}
			t.Chdir(root)
			var out bytes.Buffer
			if err := g.runSkillGate(&out, "claude"); err == nil {
				t.Fatal("drift accepted")
			}
			kind := "content"
			if stray {
				kind = "unexpected"
			}
			if !strings.Contains(out.String(), "boss/toolbox/x.mjs ("+kind+", behind)") {
				t.Fatalf("direction: %s", &out)
			}
			out.Reset()
			if err := g.runSkillGateWithHeal(&out, "claude", true); err != nil {
				t.Fatalf("heal: %v %s", err, &out)
			}
			if !strings.Contains(out.String(), "healed 1 path(s) from origin/HEAD merge-base") || strings.Contains(out.String(), "  - ") {
				t.Fatalf("heal output: %s", &out)
			}
			if stray {
				if _, err := os.Stat(stale); !os.IsNotExist(err) {
					t.Fatalf("stray survives: %v", err)
				}
			} else if got, err := os.ReadFile(stale); err != nil || string(got) != "current root\n" {
				t.Fatalf("content: %q %v", got, err)
			}
			out.Reset()
			if err := g.runSkillGate(&out, "claude"); err != nil {
				t.Fatalf("second check: %v %s", err, &out)
			}
			// A never-committed installed file must remain withheld and untouched.
			if err := os.WriteFile(stale, []byte("operator bytes\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			out.Reset()
			if err := g.runSkillGateWithHeal(&out, "claude", true); err == nil {
				t.Fatal("operator edit accepted")
			}
			if !strings.Contains(out.String(), "unrecoverable") || !strings.Contains(out.String(), "reinstall withheld [lost]") {
				t.Fatalf("refusal: %s", &out)
			}
			if got, err := os.ReadFile(stale); err != nil || string(got) != "operator bytes\n" {
				t.Fatalf("operator edit changed: %q %v", got, err)
			}
		})
	}
}

func TestSkillDriftHistoryBatch(t *testing.T) {
	g := newTestGroup()
	for _, scenario := range []string{"behind", "ahead", "exhausted", "lossless"} {
		t.Run(scenario, func(t *testing.T) {
			calls := map[string]int{}
			g.skillDriftHistoryGit = func(_ string, args ...string) (string, error) {
				if args[0] == "hash-object" {
					return "installed-blob", nil
				}
				rev := "HEAD"
				for _, arg := range args {
					if arg == "HEAD..origin/HEAD" {
						rev = arg
					}
				}
				calls[rev]++
				if scenario == "exhausted" {
					return strings.Repeat("commit\n", skillDriftHistoryLookback), nil
				}
				if scenario == "ahead" && rev == "HEAD" {
					return "", nil
				}
				return "commit\n:100644 100644 old-blob installed-blob M\tx\n", nil
			}
			kind := libskillinstall.DriftContent
			want := skillDriftBehind
			if scenario == "ahead" {
				want = skillDriftAhead
			}
			if scenario == "exhausted" {
				want = skillDriftUnknown
			}
			if scenario == "lossless" {
				kind = libskillinstall.DriftAbsent
				want = skillDriftLossless
			}
			entries := []libskillinstall.DriftEntry{{Path: "boss/a", Kind: kind}, {Path: "boss/b", Kind: kind}, {Path: "boss/c", Kind: kind}}
			if scenario == "lossless" {
				entries[1].Kind = libskillinstall.DriftMode
			}
			for _, got := range g.skillDriftEntriesWithDirection("repo", "installed", entries) {
				if got.direction != want {
					t.Fatalf("direction = %q, want %q", got.direction, want)
				}
			}
			if scenario == "lossless" && len(calls) != 0 {
				t.Fatalf("lossless history calls: %v", calls)
			}
			for rev, n := range calls {
				if n > 1 {
					t.Fatalf("%s walked %d times", rev, n)
				}
			}
		})
	}
}
