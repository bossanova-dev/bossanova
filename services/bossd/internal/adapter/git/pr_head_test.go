package git

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/rs/zerolog"
)

func TestInspectPRHead_Sessions_AC248303(t *testing.T) {
	repo := initTestRepo(t)
	mgr := NewManager(zerolog.Nop())
	gitOutput(t, repo, "checkout", "-b", "dependency")
	if err := os.WriteFile(filepath.Join(repo, "old.txt"), []byte("original"), 0o600); err != nil {
		t.Fatal(err)
	}
	gitOutput(t, repo, "add", "old.txt")
	gitOutput(t, repo, "commit", "-m", "dependency")
	gitOutput(t, repo, "push", "origin", "dependency")
	head := gitOutput(t, repo, "rev-parse", "HEAD")
	got, err := mgr.InspectPRHead(t.Context(), repo, "main", "dependency", head)
	if err != nil || !got.ContainsBase || got.BaseSHA == "" || !reflect.DeepEqual(got.ChangedFiles, []string{"old.txt"}) {
		t.Fatalf("ahead: %+v %v", got, err)
	}
	gitOutput(t, repo, "checkout", "main")
	gitOutput(t, repo, "commit", "--allow-empty", "-m", "base advanced")
	gitOutput(t, repo, "push", "origin", "main")
	got, err = mgr.InspectPRHead(t.Context(), repo, "main", "dependency", head)
	if err != nil || got.ContainsBase {
		t.Fatalf("behind: %+v %v", got, err)
	}
	gitOutput(t, repo, "checkout", "dependency")
	gitOutput(t, repo, "rebase", "main")
	gitOutput(t, repo, "push", "--force", "origin", "dependency")
	head = gitOutput(t, repo, "rev-parse", "HEAD")
	got, err = mgr.InspectPRHead(t.Context(), repo, "main", "dependency", head)
	if err != nil || !got.ContainsBase {
		t.Fatalf("rebased: %+v %v", got, err)
	}
	// A locally present commit from another branch cannot stand in for the fetched head.
	gitOutput(t, repo, "checkout", "-b", "other")
	gitOutput(t, repo, "commit", "--allow-empty", "-m", "other")
	other := gitOutput(t, repo, "rev-parse", "HEAD")
	for _, sha := range []string{other, strings.Repeat("f", 40), "HEAD"} {
		if _, err := mgr.InspectPRHead(t.Context(), repo, "main", "dependency", sha); err == nil {
			t.Fatalf("accepted unfetched head %s", sha)
		}
	}
}

func TestInspectPRHeadRenameAndFetchFailure_Sessions_AC248305(t *testing.T) {
	repo := initTestRepo(t)
	mgr := NewManager(zerolog.Nop())
	if err := os.WriteFile(filepath.Join(repo, "README.md"), []byte("base"), 0o600); err != nil {
		t.Fatal(err)
	}
	gitOutput(t, repo, "add", "README.md")
	gitOutput(t, repo, "commit", "-m", "base file")
	gitOutput(t, repo, "push", "origin", "main")
	gitOutput(t, repo, "checkout", "-b", "dependency")
	gitOutput(t, repo, "mv", "README.md", "renamed.md")
	gitOutput(t, repo, "commit", "-m", "rename")
	gitOutput(t, repo, "push", "origin", "dependency")
	head := gitOutput(t, repo, "rev-parse", "HEAD")
	got, err := mgr.InspectPRHead(t.Context(), repo, "main", "dependency", head)
	if err != nil || !reflect.DeepEqual(got.ChangedFiles, []string{"README.md", "renamed.md"}) {
		t.Fatalf("rename: %+v %v", got, err)
	}
	if _, err := mgr.InspectPRHead(t.Context(), repo, "main", "missing", head); err == nil {
		t.Fatal("missing remote branch accepted")
	}
}

func TestInspectPRHeadWhitespacePaths_Sessions_AC248302(t *testing.T) {
	for _, name := range []string{" app.json", "\tapp.json", "app\n.json"} {
		t.Run(name, func(t *testing.T) {
			repo := initTestRepo(t)
			gitOutput(t, repo, "checkout", "-b", "dependency")
			if err := os.WriteFile(filepath.Join(repo, name), []byte("dependency"), 0o600); err != nil {
				t.Fatal(err)
			}
			gitOutput(t, repo, "add", "--", name)
			gitOutput(t, repo, "commit", "-m", "whitespace path")
			gitOutput(t, repo, "push", "origin", "dependency")
			head := gitOutput(t, repo, "rev-parse", "HEAD")
			got, err := NewManager(zerolog.Nop()).InspectPRHead(t.Context(), repo, "main", "dependency", head)
			if err != nil || !reflect.DeepEqual(got.ChangedFiles, []string{name}) {
				t.Fatalf("changed paths: %+v %v; want %q", got, err, name)
			}
		})
	}
}
