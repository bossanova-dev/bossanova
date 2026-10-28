package skills

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestGitHubAdminChecks_BossInit_AC13(t *testing.T) {
	realNode(t)
	cases := []struct{ name, script, want string }{
		{"missing", "", "gh-missing"},
		{"outdated", "echo 'gh version 2.87.0'", "gh-outdated"},
		{"unauthenticated", "case \"$1\" in --version) echo 'gh version 2.88.0';; auth) exit 1;; esac", "gh auth login --hostname example.com"},
		{"unavailable", "case \"$1\" in --version) echo 'gh version 2.88.0';; repo) exit 1;; esac", "repository-unavailable"},
		{"disabled", "case \"$1\" in --version) echo 'gh version 2.88.0';; repo) echo '{\"nameWithOwner\":\"o/r\",\"url\":\"https://example.com/o/r\",\"hasIssuesEnabled\":false}';; esac", "issues-disabled"},
		{"enterprise", "case \"$1\" in --version) echo 'gh version 2.88.0';; repo) echo '{\"nameWithOwner\":\"o/r\",\"url\":\"https://example.com/o/r\",\"hasIssuesEnabled\":true}';; esac", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			if tc.script != "" {
				if err := os.WriteFile(filepath.Join(dir, "gh"), []byte("#!/bin/sh\n"+tc.script+"\n"), 0o755); err != nil {
					t.Fatal(err)
				}
				t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
			} else {
				t.Setenv("PATH", dir)
			}
			repo, err := (&ghGitHubAdmin{}).Check(context.Background(), t.TempDir(), "example.com/o/r")
			if tc.want != "" {
				if err == nil || !strings.Contains(err.Error(), tc.want) {
					t.Fatalf("error=%v; want %s", err, tc.want)
				}
				return
			}
			if err != nil || repo.Repository != "example.com/o/r" || repo.Host != "example.com" {
				t.Fatalf("repo=%+v error=%v", repo, err)
			}
		})
	}
}

func TestGitHubAdminResolvesCheckoutRepository(t *testing.T) {
	realNode(t)
	for _, host := range []string{"github.com", "github.example.com"} {
		t.Run(host, func(t *testing.T) {
			dir := t.TempDir()
			script := `#!/bin/sh
case "$1" in
 --version) echo 'gh version 2.88.0';;
 auth) test "$2 $3 $4" = 'status --hostname ` + host + `';;
 repo) test "$2 $3 $4" = 'view --json nameWithOwner,url,hasIssuesEnabled' || exit 1
 echo '{"nameWithOwner":"owner/project","url":"https://` + host + `/owner/project","hasIssuesEnabled":true}';;
 *) exit 1;;
esac
`
			for name, body := range map[string]string{"gh": script, "git": "#!/bin/sh\necho 'git@" + host + ":owner/project.git'\n"} {
				if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o755); err != nil {
					t.Fatal(err)
				}
			}
			t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
			repo, err := (&ghGitHubAdmin{}).Check(context.Background(), t.TempDir(), "")
			want := "owner/project"
			if host != "github.com" {
				want = host + "/" + want
			}
			if err != nil || repo.Repository != want || repo.Host != host {
				t.Fatalf("repo=%+v error=%v", repo, err)
			}
		})
	}
}
