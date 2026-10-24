package verifyadmission

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestAdmissionExcluded(t *testing.T) {
	head := strings.Repeat("a", 40)
	for _, tc := range []struct {
		name, entries string
		want          bool
	}{
		{"excluded", `"one":{"prs":{"3550":{"head":"` + head + `","state":"excluded","reason":"no-ticket"}}}`, true},
		{"uppercase head", `"one":{"prs":{"3550":{"head":"` + strings.ToUpper(head) + `","state":"excluded","reason":"no-ticket"}}}`, true},
		{"all excluded", `"one":{"prs":{"3550":{"head":"` + head + `","state":"excluded"}}},"two":{"prs":{"3550":{"head":"` + head + `","state":"excluded"}}}`, true},
		{"mixed admitted", `"one":{"prs":{"3550":{"head":"` + head + `","state":"excluded"}}},"two":{"prs":{"3550":{"head":"` + head + `","state":"admitted"}}}`, false},
		{"mixed pending", `"one":{"prs":{"3550":{"head":"` + head + `","state":"excluded"}}},"two":{"prs":{"3550":{"head":"` + head + `","state":"pending"}}}`, false},
		{"unknown state", `"one":{"prs":{"3550":{"head":"` + head + `","state":"unknown"}}}`, false},
		{"old head", `"one":{"prs":{"3550":{"head":"old","state":"excluded"}}}`, false},
		{"absent PR", `"one":{"prs":{"3551":{"head":"` + head + `","state":"excluded"}}}`, false},
		{"empty", ``, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			file := filepath.Join(t.TempDir(), "admission.json")
			if err := os.WriteFile(file, []byte(`{"version":1,"scopes":{`+tc.entries+`}}`), 0600); err != nil {
				t.Fatal(err)
			}
			snapshot, err := Read(file)
			if err != nil {
				t.Fatal(err)
			}
			if _, ok := snapshot.Excluded(3550, head); ok != tc.want {
				t.Fatalf("Excluded=%v want %v", ok, tc.want)
			}
			if _, ok := snapshot.Excluded(3550, ""); ok {
				t.Fatal("empty head excluded")
			}
		})
	}
}

func TestAdmissionUnreadableIsUnknown(t *testing.T) {
	for _, content := range []string{`{"version":2}`, `{`, `null`, `{"version":1,"scopes":{"s":{"prs":{"3550":{"head":9}}}}}`} {
		file := filepath.Join(t.TempDir(), "admission.json")
		if err := os.WriteFile(file, []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
		snapshot, _ := Read(file)
		if _, ok := snapshot.Excluded(3550, strings.Repeat("a", 40)); ok {
			t.Fatalf("invalid %q excluded", content)
		}
	}
	for _, path := range []string{filepath.Join(t.TempDir(), "missing"), t.TempDir()} {
		snapshot, _ := Read(path)
		if _, ok := snapshot.Excluded(3550, strings.Repeat("a", 40)); ok {
			t.Fatal("unreadable excluded")
		}
	}
}

func TestAdmissionFixture(t *testing.T) {
	snapshot, err := Read("testdata/verify-admission.v1.json")
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		pr   int
		head string
		want bool
	}{{3550, "a", true}, {3551, "b", false}, {3552, "c", false}, {3553, "d", true}, {3550, "b", false}, {9999, "a", false}} {
		reason, ok := snapshot.Excluded(tc.pr, strings.Repeat(tc.head, 40))
		if ok != tc.want || (ok && reason != "no-ticket") {
			t.Errorf("PR%d: reason=%q excluded=%v want %v", tc.pr, reason, ok, tc.want)
		}
	}
}

func TestAdmissionPathUsesWorktreeCommonDir(t *testing.T) {
	repo := t.TempDir()
	runGit := func(args ...string) string {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", repo}, args...)...)
		cmd.WaitDelay = time.Second
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	runGit("init")
	runGit("-c", "user.name=Test", "-c", "user.email=test@example.com", "-c", "commit.gpgsign=false", "commit", "--allow-empty", "-m", "fixture")
	worktree := filepath.Join(t.TempDir(), "worktree")
	runGit("worktree", "add", "--detach", worktree)
	want := filepath.Join(runGit("rev-parse", "--path-format=absolute", "--git-common-dir"), "boss-verify", "verify-admission.json")
	for _, dir := range []string{repo, worktree} {
		got, err := Path(context.Background(), dir)
		if err != nil {
			t.Fatal(err)
		}
		if got != want {
			t.Errorf("Path(%s)=%s want %s", dir, got, want)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := Path(ctx, repo); err == nil {
		t.Fatal("cancelled lookup succeeded")
	}
	if _, err := Path(context.Background(), t.TempDir()); err == nil {
		t.Fatal("non-repo lookup succeeded")
	}
}
