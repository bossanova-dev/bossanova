package git

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"net/http"
	"net/http/cgi"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rs/zerolog"
)

// fixtureUser is the HTTP basic-auth user gitHTTPFixture requires.
const fixtureUser = "boss-user"

// gitHTTPFixture serves the bare repos under root over loopback HTTP through
// git-http-backend, requiring HTTP basic auth as fixtureUser/password.
func gitHTTPFixture(t *testing.T, root, password string) *httptest.Server {
	t.Helper()
	return gitHTTPFixtureUser(t, root, fixtureUser, password)
}

func gitHTTPFixtureUser(t *testing.T, root, user, password string) *httptest.Server {
	t.Helper()
	execPath, err := exec.Command("git", "--exec-path").Output()
	if err != nil {
		t.Fatalf("git --exec-path: %v", err)
	}
	backend := filepath.Join(strings.TrimSpace(string(execPath)), "git-http-backend")
	if _, err := os.Stat(backend); err != nil {
		t.Fatalf("git-http-backend not found at %s: %v", backend, err)
	}
	cgiHandler := &cgi.Handler{
		Path: backend,
		Env:  []string{"GIT_PROJECT_ROOT=" + root, "GIT_HTTP_EXPORT_ALL=1"},
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u, p, ok := r.BasicAuth()
		if !ok || u != user || p != password {
			w.Header().Set("WWW-Authenticate", `Basic realm="boss-test"`)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		cgiHandler.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// recordCloneArgv wraps the Clone runner seam, recording every git argv.
func recordCloneArgv(t *testing.T) func() [][]string {
	t.Helper()
	var mu sync.Mutex
	var calls [][]string
	previous := runGitWithTimeoutFn
	runGitWithTimeoutFn = func(ctx context.Context, timeout time.Duration, dir string, args ...string) (string, error) {
		mu.Lock()
		calls = append(calls, append([]string(nil), args...))
		mu.Unlock()
		return previous(ctx, timeout, dir, args...)
	}
	t.Cleanup(func() { runGitWithTimeoutFn = previous })
	return func() [][]string {
		mu.Lock()
		defer mu.Unlock()
		return append([][]string(nil), calls...)
	}
}

// hermeticGitEnv keeps the developer's global and system git config (notably a
// credential.helper such as osxkeychain) out of the test, and makes git fail
// rather than prompt.
func hermeticGitEnv(t *testing.T) {
	t.Helper()
	t.Setenv("GIT_CONFIG_GLOBAL", filepath.Join(t.TempDir(), "gitconfig"))
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	t.Setenv("GIT_TERMINAL_PROMPT", "0")
	t.Setenv("GIT_ASKPASS", "")
	t.Setenv("SSH_ASKPASS", "")
}

func markerPassword(t *testing.T) string {
	t.Helper()
	b := make([]byte, 12)
	if _, err := rand.Read(b); err != nil {
		t.Fatalf("rand: %v", err)
	}
	return "marker-" + hex.EncodeToString(b)
}

// bareFixtureRepo returns the projects root and the name of a bare repo under
// it that holds one commit on main.
func bareFixtureRepo(t *testing.T) (root, name string) {
	t.Helper()
	work := initTestRepo(t)
	bare, err := exec.Command("git", "-C", work, "remote", "get-url", "origin").Output()
	if err != nil {
		t.Fatalf("read fixture origin: %v", err)
	}
	bareDir := strings.TrimSpace(string(bare))
	return filepath.Dir(bareDir), filepath.Base(bareDir)
}

// TestCloneWithUserinfoKeepsCredentialsOutOfArgv is the BOS-1587 contract for
// Manager.Clone: a URL's credentials never reach git's argv or .git/config, are
// kept in a 0600 .git/boss-credentials file, and later fetches authenticate.
func TestCloneWithUserinfoKeepsCredentialsOutOfArgv(t *testing.T) {
	hermeticGitEnv(t)
	root, name := bareFixtureRepo(t)
	password := markerPassword(t)
	srv := gitHTTPFixture(t, root, password)

	cloneURL, err := url.Parse(srv.URL + "/" + name)
	if err != nil {
		t.Fatalf("parse fixture url: %v", err)
	}
	cloneURL.User = url.UserPassword(fixtureUser, password)

	calls := recordCloneArgv(t)
	dest := filepath.Join(t.TempDir(), "clone")
	mgr := NewManager(zerolog.Nop())
	if err := mgr.Clone(context.Background(), cloneURL.String(), dest); err != nil {
		t.Fatalf("Clone: %v", err)
	}

	recorded := calls()
	if len(recorded) == 0 {
		t.Fatal("no git argv recorded through the Clone runner seam")
	}
	var credDirs []string
	for _, argv := range recorded {
		joined := strings.Join(argv, " ")
		if strings.Contains(joined, password) {
			t.Fatalf("clone credentials reached git argv: %s", redactedGitArgv(argv))
		}
		for _, a := range argv {
			if i := strings.Index(a, "--file="); i >= 0 {
				credDirs = append(credDirs, filepath.Dir(strings.Trim(a[i+len("--file="):], "'")))
			}
		}
	}

	gitConfig, err := os.ReadFile(filepath.Join(dest, ".git", "config"))
	if err != nil {
		t.Fatalf("read .git/config: %v", err)
	}
	if strings.Contains(string(gitConfig), password) {
		t.Fatal("clone credentials were written to .git/config")
	}

	credPath := filepath.Join(dest, ".git", "boss-credentials")
	info, err := os.Stat(credPath)
	if err != nil {
		t.Fatalf("stat .git/boss-credentials: %v", err)
	}
	if mode := info.Mode().Perm(); mode != 0o600 {
		t.Fatalf(".git/boss-credentials mode = %o, want 600", mode)
	}

	// The clone-time temp credential directory is gone.
	for _, dir := range credDirs {
		if filepath.Dir(dir) == filepath.Join(dest, ".git") || dir == filepath.Join(dest, ".git") {
			continue
		}
		if _, err := os.Stat(dir); !os.IsNotExist(err) {
			t.Fatalf("clone-time credential directory %s still exists (err %v)", dir, err)
		}
	}

	// A later fetch, run as an ordinary git command in the clone, still
	// authenticates through the repo-local helper.
	fetch := exec.Command("git", "-C", dest, "fetch", "origin")
	if out, err := fetch.CombinedOutput(); err != nil {
		t.Fatalf("fetch after clone did not authenticate: %v\n%s", err, redactURLCredentials(string(out)))
	}
}

// TestCloneWithUserinfoUnderUseHTTPPath: with credential.useHttpPath=true
// (common for Azure DevOps) git's credential query carries the repo path, and
// credential-store only matches an entry carrying that same path. The clone
// and a later fetch must both still authenticate.
func TestCloneWithUserinfoUnderUseHTTPPath(t *testing.T) {
	hermeticGitEnv(t)
	if err := os.WriteFile(os.Getenv("GIT_CONFIG_GLOBAL"),
		[]byte("[credential]\n\tuseHttpPath = true\n"), 0o600); err != nil {
		t.Fatalf("write global git config: %v", err)
	}
	root, name := bareFixtureRepo(t)
	password := markerPassword(t)
	srv := gitHTTPFixture(t, root, password)

	cloneURL, err := url.Parse(srv.URL + "/" + name)
	if err != nil {
		t.Fatalf("parse fixture url: %v", err)
	}
	cloneURL.User = url.UserPassword(fixtureUser, password)

	dest := filepath.Join(t.TempDir(), "clone")
	if err := NewManager(zerolog.Nop()).Clone(context.Background(), cloneURL.String(), dest); err != nil {
		t.Fatalf("Clone with credential.useHttpPath=true: %v", redactURLCredentials(err.Error()))
	}
	fetch := exec.Command("git", "-C", dest, "fetch", "origin")
	if out, err := fetch.CombinedOutput(); err != nil {
		t.Fatalf("fetch with credential.useHttpPath=true did not authenticate: %v\n%s", err, redactURLCredentials(string(out)))
	}
}

// TestCloneKeepCredentialsFailureRemovesClone: when the clone succeeds but its
// credential helper cannot be configured, Clone fails AND removes the clone it
// created, so a retry clones again instead of registering a repo that cannot
// authenticate.
func TestCloneKeepCredentialsFailureRemovesClone(t *testing.T) {
	hermeticGitEnv(t)
	root, name := bareFixtureRepo(t)
	password := markerPassword(t)
	srv := gitHTTPFixture(t, root, password)
	cloneURL, err := url.Parse(srv.URL + "/" + name)
	if err != nil {
		t.Fatalf("parse fixture url: %v", err)
	}
	cloneURL.User = url.UserPassword(fixtureUser, password)

	previous := runGitWithTimeoutFn
	t.Cleanup(func() { runGitWithTimeoutFn = previous })
	runGitWithTimeoutFn = func(ctx context.Context, timeout time.Duration, dir string, args ...string) (string, error) {
		if len(args) > 0 && args[0] == "config" {
			return "", errors.New("injected credential.helper config failure")
		}
		return previous(ctx, timeout, dir, args...)
	}

	dest := filepath.Join(t.TempDir(), "clone")
	mgr := NewManager(zerolog.Nop())
	if err := mgr.Clone(context.Background(), cloneURL.String(), dest); err == nil {
		t.Fatal("Clone succeeded although its credential helper could not be configured")
	}
	if _, err := os.Stat(dest); !os.IsNotExist(err) {
		t.Fatalf("clone directory left behind after keeping credentials failed (stat err %v)", err)
	}

	// A directory Clone did not create is never removed: git clone also
	// accepts an existing empty directory, which may be the caller's.
	existing := filepath.Join(t.TempDir(), "existing")
	if err := os.Mkdir(existing, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := mgr.Clone(context.Background(), cloneURL.String(), existing); err == nil {
		t.Fatal("Clone into an existing directory succeeded although its credential helper could not be configured")
	}
	if info, err := os.Stat(existing); err != nil || !info.IsDir() {
		t.Fatalf("Clone removed a directory it did not create (stat err %v)", err)
	}

	// The retry clones again and authenticates.
	runGitWithTimeoutFn = previous
	if err := mgr.Clone(context.Background(), cloneURL.String(), dest); err != nil {
		t.Fatalf("retry Clone: %v", redactURLCredentials(err.Error()))
	}
	if out, err := exec.Command("git", "-C", dest, "fetch", "origin").CombinedOutput(); err != nil {
		t.Fatalf("fetch after retry did not authenticate: %v\n%s", err, redactURLCredentials(string(out)))
	}
}

// TestCloneWithUserinfoWrongPasswordCleansUp: a refused clone returns an error
// that does not carry the password and leaves no temp credential directory.
func TestCloneWithUserinfoWrongPasswordCleansUp(t *testing.T) {
	hermeticGitEnv(t)
	root, name := bareFixtureRepo(t)
	srv := gitHTTPFixture(t, root, markerPassword(t))

	wrong := markerPassword(t)
	cloneURL, err := url.Parse(srv.URL + "/" + name)
	if err != nil {
		t.Fatalf("parse fixture url: %v", err)
	}
	cloneURL.User = url.UserPassword(fixtureUser, wrong)

	calls := recordCloneArgv(t)
	dest := filepath.Join(t.TempDir(), "clone")
	err = NewManager(zerolog.Nop()).Clone(context.Background(), cloneURL.String(), dest)
	if err == nil {
		t.Fatal("Clone with a wrong password succeeded")
	}
	if strings.Contains(err.Error(), wrong) {
		t.Fatal("Clone error carries the password")
	}
	for _, argv := range calls() {
		if strings.Contains(strings.Join(argv, " "), wrong) {
			t.Fatal("clone credentials reached git argv")
		}
		for _, a := range argv {
			if i := strings.Index(a, "--file="); i >= 0 {
				dir := filepath.Dir(strings.Trim(a[i+len("--file="):], "'"))
				if _, statErr := os.Stat(dir); !os.IsNotExist(statErr) {
					t.Fatalf("temp credential directory %s survived a failed clone", dir)
				}
			}
		}
	}
}

// TestCloneWithoutUserinfoPassesURLThrough: a URL with no credentials is
// cloned with the URL as given, after a "--" separator, and no helper.
func TestCloneWithoutUserinfoPassesURLThrough(t *testing.T) {
	hermeticGitEnv(t)
	root, name := bareFixtureRepo(t)
	src := filepath.Join(root, name)

	calls := recordCloneArgv(t)
	dest := filepath.Join(t.TempDir(), "clone")
	if err := NewManager(zerolog.Nop()).Clone(context.Background(), src, dest); err != nil {
		t.Fatalf("Clone: %v", err)
	}
	recorded := calls()
	if len(recorded) != 1 {
		t.Fatalf("git invocations = %d, want 1: %v", len(recorded), recorded)
	}
	want := []string{"clone", "--", src, dest}
	if strings.Join(recorded[0], "\x00") != strings.Join(want, "\x00") {
		t.Fatalf("argv = %q, want %q", recorded[0], want)
	}
	if _, err := os.Stat(filepath.Join(dest, ".git", "boss-credentials")); !os.IsNotExist(err) {
		t.Fatalf("credential file created for a URL without credentials (err %v)", err)
	}
}

func TestCloneWithTokenUsernameKeepsTokenOutOfArgv(t *testing.T) {
	for _, usePath := range []string{"false", "true"} {
		t.Run("useHttpPath="+usePath, func(t *testing.T) {
			hermeticGitEnv(t)
			if err := os.WriteFile(os.Getenv("GIT_CONFIG_GLOBAL"), []byte("[credential]\nuseHttpPath = "+usePath+"\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			root, name := bareFixtureRepo(t)
			token := "ghp_" + strings.TrimPrefix(markerPassword(t), "marker-")
			srv := gitHTTPFixtureUser(t, root, token, "")
			cloneURL, err := url.Parse(srv.URL + "/" + name)
			if err != nil {
				t.Fatal(err)
			}
			cloneURL.User = url.User(token)
			calls := recordCloneArgv(t)
			dest := filepath.Join(t.TempDir(), "clone")
			mgr := NewManager(zerolog.Nop())
			if err := mgr.Clone(context.Background(), cloneURL.String(), dest); err != nil {
				t.Fatalf("Clone: %v", redactURLCredentials(err.Error()))
			}
			if len(calls()) == 0 {
				t.Fatal("no clone argv recorded")
			}
			for _, argv := range calls() {
				if strings.Contains(strings.Join(argv, " "), token) {
					t.Fatal("token reached git argv")
				}
			}
			config, err := os.ReadFile(filepath.Join(dest, ".git", "config"))
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(config), token) {
				t.Fatal("token reached .git/config")
			}
			info, err := os.Stat(filepath.Join(dest, ".git", cloneCredentialFileName))
			if err != nil {
				t.Fatal(err)
			}
			if info.Mode().Perm() != 0o600 {
				t.Fatalf("credential mode = %o, want 600", info.Mode().Perm())
			}
			if CloneURLWithoutCredentials(cloneURL.String()) != srv.URL+"/"+name {
				t.Fatal("recorded origin contains userinfo")
			}
			origin, err := mgr.DetectOriginURL(context.Background(), dest)
			if err != nil {
				t.Fatal(err)
			}
			if origin != srv.URL+"/"+name {
				t.Fatal("detected origin contains userinfo")
			}
			if out, err := exec.Command("git", "-C", dest, "fetch", "origin").CombinedOutput(); err != nil {
				t.Fatalf("fetch: %v\n%s", err, redactURLCredentials(string(out)))
			}
		})
	}
}

func TestCloneWithPlainUsernameReachesConfiguredHelper(t *testing.T) {
	hermeticGitEnv(t)
	root, name := bareFixtureRepo(t)
	password := markerPassword(t)
	srv := gitHTTPFixtureUser(t, root, "alice", password)
	cloneURL, err := url.Parse(srv.URL + "/" + name)
	if err != nil {
		t.Fatal(err)
	}
	cloneURL.User = url.UserPassword("alice", password)
	store := filepath.Join(t.TempDir(), "credentials")
	if err := os.WriteFile(store, []byte(cloneURL.String()+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("git", "config", "--global", "credential.helper", credentialStoreHelper(store)).CombinedOutput(); err != nil {
		t.Fatalf("configure helper: %v\n%s", err, out)
	}
	cloneURL.User = url.User("alice")
	calls := recordCloneArgv(t)
	dest := filepath.Join(t.TempDir(), "clone")
	if err := NewManager(zerolog.Nop()).Clone(context.Background(), cloneURL.String(), dest); err != nil {
		t.Fatal(err)
	}
	got := calls()
	want := []string{"clone", "--", cloneURL.String(), dest}
	if len(got) != 1 || strings.Join(got[0], "\x00") != strings.Join(want, "\x00") {
		t.Fatalf("clone argv = %q, want %q", got, want)
	}
	if _, err := os.Stat(filepath.Join(dest, ".git", cloneCredentialFileName)); !os.IsNotExist(err) {
		t.Fatalf("unexpected credential file: %v", err)
	}
	if out, err := exec.Command("git", "-C", dest, "fetch", "origin").CombinedOutput(); err != nil {
		t.Fatalf("fetch: %v\n%s", err, redactURLCredentials(string(out)))
	}
}

// TestSplitCloneCredentials distinguishes token-shaped from ordinary usernames.
func TestSplitCloneCredentials(t *testing.T) {
	pw := "pw-" + "marker"
	cases := []struct {
		name, raw, wantPlain, wantCred string
		wantOK                         bool
	}{
		{"user and password", "https://alice:" + pw + "@example.com/o/r.git",
			"https://example.com/o/r.git", "https://alice:" + pw + "@example.com/o/r.git", true},
		{"password only", "https://:" + pw + "@example.com/o/r.git",
			"https://example.com/o/r.git", "https://:" + pw + "@example.com/o/r.git", true},
		{"no path", "https://alice:" + pw + "@example.com",
			"https://example.com", "https://alice:" + pw + "@example.com", true},
		{"query is part of the credential", "https://alice:" + pw + "@example.com/o/r.git?x=1",
			"https://example.com/o/r.git?x=1", "https://alice:" + pw + "@example.com/o/r.git?x=1", true},
		{"fragment is part of the credential", "https://alice:" + pw + "@example.com/o/r.git?x=1#frag",
			"https://example.com/o/r.git?x=1#frag", "https://alice:" + pw + "@example.com/o/r.git?x=1#frag", true},
		{"username only", "https://alice@example.com/o/r.git",
			"https://alice@example.com/o/r.git", "", false},
		{"GitHub token", "https://ghp_marker@example.com/o/r.git?x=1#frag", "https://example.com/o/r.git?x=1#frag", "https://ghp_marker:@example.com/o/r.git?x=1#frag", true},
		{"GitLab token", "https://glpat-marker@example.com/o/r.git", "https://example.com/o/r.git", "https://glpat-marker:@example.com/o/r.git", true},
		{"40 hex token", "https://" + strings.Repeat("a", 40) + "@example.com/o/r.git", "https://example.com/o/r.git", "https://" + strings.Repeat("a", 40) + ":@example.com/o/r.git", true},
		{"39 character username", "https://" + strings.Repeat("a", 39) + "@example.com/o/r.git", "https://" + strings.Repeat("a", 39) + "@example.com/o/r.git", "", false},
		{"email username", "https://a.b%40x.com@example.com/o/r.git", "https://a.b%40x.com@example.com/o/r.git", "", false},
		{"ssh token", "ssh://ghp_marker@example.com/o/r.git", "ssh://ghp_marker@example.com/o/r.git", "", false},
		{"no userinfo", "https://example.com/o/r.git", "https://example.com/o/r.git", "", false},
		{"ssh login name", "ssh://git@example.com/o/r.git", "ssh://git@example.com/o/r.git", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			plain, cred, ok := splitCloneCredentials(tc.raw)
			if plain != tc.wantPlain || cred != tc.wantCred || ok != tc.wantOK {
				t.Fatalf("splitCloneCredentials(%q) = (%q, %q, %v), want (%q, %q, %v)",
					tc.raw, plain, cred, ok, tc.wantPlain, tc.wantCred, tc.wantOK)
			}
		})
	}
}

// TestSplitCloneCredentialsMatchUnderUseHTTPPath: git looks the stored
// credential up by the URL it fetches. With credential.useHttpPath=true that
// lookup carries everything after the host — path, query string and fragment
// (git 2.53) — so the stored line must carry all three or credential-store
// finds nothing; without it the same line still matches.
func TestSplitCloneCredentialsMatchUnderUseHTTPPath(t *testing.T) {
	hermeticGitEnv(t)
	password := markerPassword(t)
	for _, raw := range []string{
		"https://alice:" + password + "@example.com/o/r.git",
		"https://alice:" + password + "@example.com/o/r.git?x=1",
		"https://alice:" + password + "@example.com/o/r.git?x=1#frag",
	} {
		plain, cred, ok := splitCloneCredentials(raw)
		if !ok {
			t.Fatalf("splitCloneCredentials did not split a credentialed URL")
		}
		credFile := filepath.Join(t.TempDir(), "credentials")
		if err := os.WriteFile(credFile, []byte(cred+"\n"), 0o600); err != nil {
			t.Fatalf("write credential file: %v", err)
		}
		for _, useHTTPPath := range []string{"true", "false"} {
			fill := exec.Command("git", "-c", "credential.helper=",
				"-c", "credential.helper="+credentialStoreHelper(credFile),
				"-c", "credential.useHttpPath="+useHTTPPath, "credential", "fill")
			fill.Stdin = strings.NewReader("url=" + plain + "\n\n")
			out, err := fill.CombinedOutput()
			if err != nil || !strings.Contains(string(out), "password="+password+"\n") {
				t.Fatalf("credential fill for %s with useHttpPath=%s did not find the stored credential (err %v)",
					redactURLCredentials(raw), useHTTPPath, err)
			}
		}
	}
}

// TestCloneURLWithoutCredentials: the URL a credentialed clone records for
// origin drops password userinfo and token-shaped usernames.
func TestCloneURLWithoutCredentials(t *testing.T) {
	pw := "pw-" + "marker"
	for raw, want := range map[string]string{
		"https://alice:" + pw + "@example.com/o/r.git":                "https://example.com/o/r.git",
		"https://ghp_marker@example.com/o/r.git":                      "https://example.com/o/r.git",
		"https://glpat-marker@example.com/o/r.git":                    "https://example.com/o/r.git",
		"https://" + strings.Repeat("a", 40) + "@example.com/o/r.git": "https://example.com/o/r.git",
		"https://alice@example.com/o/r.git":                           "https://alice@example.com/o/r.git",
		"git@github.com:o/r.git":                                      "git@github.com:o/r.git",
		"https://example.com/o/r.git":                                 "https://example.com/o/r.git",
	} {
		if got := CloneURLWithoutCredentials(raw); got != want {
			t.Errorf("CloneURLWithoutCredentials(%q) = %q, want %q", raw, got, want)
		}
	}
}

func TestIsTokenShapedUsername(t *testing.T) {
	cases := []struct {
		name string
		want bool
	}{
		{"", false}, {"alice", false}, {strings.Repeat("a", 39), false},
		{strings.Repeat("a", 40), true}, {strings.Repeat("A0_-", 13), true},
		{strings.Repeat("a", 40) + ".", false}, {strings.Repeat("a", 40) + "é", false},
		{"a.b@x.com", false}, {"GHP_marker", false}, {"GLPAT-marker", false},
		{"ghp_marker", true}, {"gho_marker", true}, {"ghu_marker", true},
		{"ghs_marker", true}, {"ghr_marker", true}, {"github_pat_marker", true},
		{"glpat-marker", true}, {"gldt-marker", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isTokenShapedUsername(tc.name); got != tc.want {
				t.Fatalf("isTokenShapedUsername(%q) = %v, want %v", tc.name, got, tc.want)
			}
		})
	}
}
