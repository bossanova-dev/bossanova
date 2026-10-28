package skills

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"

	libskillinstall "github.com/recurser/bossalib/contract/plugin/skillinstall"
)

func assertCheckoutStaleOutput(t *testing.T, got string, behind int) {
	t.Helper()
	assertGateOutputHidesGateFlag(t, got)
	if strings.Count(got, "boss skills gate: claude ") != 1 || !strings.Contains(got, "checkout-stale") || !strings.Contains(got, "origin/HEAD") || strings.Contains(got, "  - ") || strings.Contains(got, "reinstall withheld") {
		t.Fatalf("stale-only output: %s", got)
	}
	if !strings.Contains(got, "1 commit(s) behind") {
		t.Fatalf("missing behind count %d: %s", behind, got)
	}
}

// The fixture installs origin's tip then resets only a disposable source checkout.
func setupOriginTipGate(t *testing.T, change string) (*Group, string, string) {
	t.Helper()
	g := newTestGroup()
	home := g.setupSkillStartupTest(t)
	root := t.TempDir()
	fixture := gateSkillFS()
	fixture["skills/boss/keep.txt"] = &fstest.MapFile{Data: []byte("keep core directory\n")}
	src := writeSkillSources(t, root, fixture)
	commitCheckoutAsOriginHead(t, root)
	path := filepath.Join(src, "skills", "boss", "SKILL.md")
	switch change {
	case "deleted":
		if err := os.Remove(path); err != nil {
			t.Fatal(err)
		}
	case "added":
		path = filepath.Join(src, "skills", "boss-new", "SKILL.md")
		if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
			t.Fatal(err)
		}
		fallthrough
	default:
		if err := os.WriteFile(path, []byte("origin tip\n"), 0644); err != nil {
			t.Fatal(err)
		}
	}
	runGit(t, root, "add", ".")
	runGit(t, root, "-c", "user.name=Test", "-c", "user.email=test@example.com", "commit", "--quiet", "-m", "origin tip")
	runGit(t, root, "update-ref", "refs/remotes/origin/main", "HEAD")
	dir := filepath.Join(home, ".claude", "skills")
	if err := libskillinstall.Extract(dir, os.DirFS(src)); err != nil {
		t.Fatal(err)
	}
	runGit(t, root, "reset", "--quiet", "--hard", "HEAD~1")
	t.Chdir(root)
	return g, root, dir
}

func TestOriginTipGate_BossSkills_AC7(t *testing.T) {
	for _, change := range []string{"content", "deleted", "added"} {
		for _, heal := range []bool{false, true} {
			t.Run(change+map[bool]string{false: "/check", true: "/heal"}[heal], func(t *testing.T) {
				g, _, dir := setupOriginTipGate(t, change)
				var out bytes.Buffer
				err := g.runSkillGateWithHeal(&out, "claude", heal)
				t.Logf("exit error=%v\n%s", err, &out)
				if err != nil {
					t.Fatalf("tip match rejected: %v\n%s", err, &out)
				}
				assertCheckoutStaleOutput(t, out.String(), 1)
				path := filepath.Join(dir, libskillinstall.Namespace, "boss", "SKILL.md")
				data, readErr := os.ReadFile(path)
				if change == "deleted" {
					if !os.IsNotExist(readErr) {
						t.Fatalf("deleted path restored: %q %v", data, readErr)
					}
				} else if readErr != nil {
					t.Fatal(readErr)
				}
			})
		}
	}
}

func TestOriginTipGateFailSafe_BossSkills_AC7(t *testing.T) {
	for _, fail := range []string{"ls-tree", "hash-object", "empty-tree"} {
		t.Run(fail, func(t *testing.T) {
			g, _, _ := setupOriginTipGate(t, "content")
			original := g.skillDriftHistoryGit
			failed := false
			g.skillDriftHistoryGit = func(root string, args ...string) (string, error) {
				if !failed && (args[0] == fail || (fail == "empty-tree" && args[0] == "ls-tree")) {
					failed = true
					if fail == "empty-tree" {
						return "", nil
					}
					return "", errors.New("git unavailable")
				}
				return original(root, args...)
			}
			var out bytes.Buffer
			if err := g.runSkillGate(&out, "claude"); err == nil {
				t.Fatalf("failed lookup accepted: %s", &out)
			}
			assertGateOutputHidesGateFlag(t, out.String())
			if strings.Contains(out.String(), "checkout-stale") || !strings.Contains(out.String(), "  - boss/SKILL.md") {
				t.Fatal(out.String())
			}
			if !strings.Contains(out.String(), "reinstall withheld [downgrade]") {
				t.Fatal(out.String())
			}
		})
	}
}

func TestOriginTipIntermediateStillDowngrade_BossSkills_AC1(t *testing.T) {
	g, root, dir := setupOriginTipGate(t, "content")
	runGit(t, root, "checkout", "--detach", "origin/HEAD")
	path := filepath.Join(root, libskillinstall.SourceRelPath, "skills", "boss", "SKILL.md")
	if err := os.WriteFile(path, []byte("later tip\n"), 0644); err != nil {
		t.Fatal(err)
	}
	runGit(t, root, "add", ".")
	runGit(t, root, "-c", "user.name=Test", "-c", "user.email=test@example.com", "commit", "--quiet", "-m", "later tip")
	runGit(t, root, "update-ref", "refs/remotes/origin/main", "HEAD")
	runGit(t, root, "reset", "--quiet", "--hard", "HEAD~2")
	var out bytes.Buffer
	if err := g.runSkillGateWithHeal(&out, "claude", true); err == nil {
		t.Fatal("intermediate accepted")
	}
	assertGateOutputHidesGateFlag(t, out.String())
	for _, want := range []string{"boss/SKILL.md (content, ahead)", "reinstall withheld [downgrade]"} {
		if !strings.Contains(out.String(), want) {
			t.Fatal(out.String())
		}
	}
	data, err := os.ReadFile(filepath.Join(dir, libskillinstall.Namespace, "boss", "SKILL.md"))
	// guidance-audit: allow — installed fixture bytes prove heal preserves intermediate revisions.
	if err != nil || string(data) != "origin tip\n" {
		t.Fatalf("intermediate changed: %q %v", data, err)
	}
}

func TestOriginTipGateRetainsCapabilities_BossSkills_AC7(t *testing.T) {
	for _, change := range []string{"mode", "broken-link"} {
		t.Run(change, func(t *testing.T) {
			g, _, dir := setupOriginTipGate(t, "content")
			if change == "mode" {
				if err := os.Chmod(filepath.Join(dir, libskillinstall.Namespace, "boss", "SKILL.md"), 0755); err != nil {
					t.Fatal(err)
				}
			} else {
				path := filepath.Join(dir, "boss")
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink("missing", path); err != nil {
					t.Fatal(err)
				}
			}
			var out bytes.Buffer
			if err := g.runSkillGate(&out, "claude"); err == nil {
				t.Fatalf("capability mismatch accepted: %s", &out)
			}
			assertGateOutputHidesGateFlag(t, out.String())
			if change == "mode" && strings.Contains(out.String(), "checkout-stale") {
				t.Fatal(out.String())
			}
			if change == "broken-link" && !strings.Contains(out.String(), "boss (broken-symlink,") {
				t.Fatal(out.String())
			}
		})
	}
}

func TestOriginTipGateNoLookupAtTip_BossSkills_AC7(t *testing.T) {
	g, root, _ := setupOriginTipGate(t, "content")
	runGit(t, root, "reset", "--quiet", "--hard", "origin/HEAD")
	original := g.skillDriftHistoryGit
	g.skillDriftHistoryGit = func(root string, args ...string) (string, error) {
		if args[0] == "ls-tree" {
			t.Error("unnecessary origin listing at current checkout")
		}
		return original(root, args...)
	}
	var out bytes.Buffer
	if err := g.runSkillGate(&out, "claude"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "clean") {
		t.Fatal(out.String())
	}
}

func TestOriginTipGateBatchesFiles_BossSkills_AC7(t *testing.T) {
	g, root, dir := setupOriginTipGate(t, "content")
	runGit(t, root, "checkout", "--detach", "origin/HEAD")
	path := filepath.Join(root, libskillinstall.SourceRelPath, "skills", "boss-build", "SKILL.md")
	if err := os.WriteFile(path, []byte("second tip file\n"), 0644); err != nil {
		t.Fatal(err)
	}
	runGit(t, root, "add", ".")
	runGit(t, root, "-c", "user.name=Test", "-c", "user.email=test@example.com", "commit", "--quiet", "-m", "second file")
	runGit(t, root, "update-ref", "refs/remotes/origin/main", "HEAD")
	if err := libskillinstall.Extract(dir, os.DirFS(filepath.Join(root, libskillinstall.SourceRelPath))); err != nil {
		t.Fatal(err)
	}
	runGit(t, root, "reset", "--quiet", "--hard", "HEAD~2")
	original := g.skillDriftHistoryGit
	hashes, listings := 0, 0
	g.skillDriftHistoryGit = func(root string, args ...string) (string, error) {
		if args[0] == "ls-tree" {
			listings++
		}
		if args[0] == "hash-object" {
			hashes++
			if len(args) != 4 {
				t.Errorf("hash batch = %v, want two file operands", args)
			}
		}
		return original(root, args...)
	}
	var out bytes.Buffer
	if err := g.runSkillGate(&out, "claude"); err != nil {
		t.Fatalf("%v: %s", err, &out)
	}
	if hashes != 1 || listings != 1 || !strings.Contains(out.String(), "2 checkout-stale path(s)") {
		t.Fatalf("hashes=%d listings=%d output=%s", hashes, listings, &out)
	}
}
