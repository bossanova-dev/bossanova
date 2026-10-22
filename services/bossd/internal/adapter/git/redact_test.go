package git

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
)

func TestRedactURLCredentials(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"user and password", "https://user:pass@github.com/o/r.git", "https://***@github.com/o/r.git"},
		{"user only (token as user)", "https://ghp_abc123@github.com/o/r.git", "https://***@github.com/o/r.git"},
		{"x-access-token form", "https://x-access-token:tok@github.com/o/r", "https://***@github.com/o/r"},
		{"ssh scheme with user", "ssh://git@github.com/o/r.git", "ssh://***@github.com/o/r.git"},
		{"embedded in a git error", "git clone https://u:p@host/x.git /tmp/x: exit status 128: fatal: could not read from https://u:p@host/x.git",
			"git clone https://***@host/x.git /tmp/x: exit status 128: fatal: could not read from https://***@host/x.git"},
		{"quoted url in stderr", "fatal: unable to access 'https://u:p@host/x.git/': Failed to connect",
			"fatal: unable to access 'https://***@host/x.git/': Failed to connect"},
		{"token query param", "https://host/x.git?token=abc&ref=main", "https://host/x.git?token=***&ref=main"},
		{"credential query params", "https://host/p?access_token=a&password=b&key=c&sig=d&keep=e",
			"https://host/p?access_token=***&password=***&key=***&sig=***&keep=e"},
		{"query param name case", "https://host/p?Token=abc", "https://host/p?Token=***"},
		{"userinfo and query", "https://u:p@host/p?sig=zz#frag", "https://***@host/p?sig=***#frag"},
		{"non-credential query untouched", "https://host/p?ref=main&keyring=x", "https://host/p?ref=main&keyring=x"},
		{"no credentials untouched", "https://github.com/o/r.git", "https://github.com/o/r.git"},
		{"scp-style untouched", "git@github.com:o/r.git", "git@github.com:o/r.git"},
		{"plain text untouched", "fatal: not a git repository (or any parent): .git", "fatal: not a git repository (or any parent): .git"},
		{"email untouched", "Author: Dev <dev@example.com>", "Author: Dev <dev@example.com>"},
		{"empty", "", ""},
		{"apostrophe in password", "https://user:sec'ret@host/repo.git", "https://***@host/repo.git"},
		{"apostrophe in token query value", "https://host/p?token=sec'ret&ref=main", "https://host/p?token=***&ref=main"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := redactURLCredentials(tc.in); got != tc.want {
				t.Fatalf("redactURLCredentials(%q)\n got %q\nwant %q", tc.in, got, tc.want)
			}
		})
	}
}

// TestRedactURLCredentialsExported pins the exported entry point other
// packages use to log a repo's origin URL.
func TestRedactURLCredentialsExported(t *testing.T) {
	in := "https://x-access-token:ghp_secret@github.com/o/r.git"
	got := RedactURLCredentials(in)
	if strings.Contains(got, "ghp_secret") || got != "https://***@github.com/o/r.git" {
		t.Fatalf("RedactURLCredentials(%q) = %q, want the userinfo redacted", in, got)
	}
}

// TestCloneErrorRedactsCredentials clones an unreachable loopback URL whose
// userinfo carries a marker password built at runtime, and asserts the marker
// reaches neither the returned error nor the logs — the Manager's own logger
// and the package-global one runGitRemote and gitCommandEnv write to.
func TestCloneErrorRedactsCredentials(t *testing.T) {
	buf := make([]byte, 12)
	if _, err := rand.Read(buf); err != nil {
		t.Fatalf("rand: %v", err)
	}
	marker := "redactmarker" + hex.EncodeToString(buf)

	var mgrLog, globalLog bytes.Buffer
	oldGlobal := log.Logger
	log.Logger = zerolog.New(&globalLog)
	t.Cleanup(func() { log.Logger = oldGlobal })

	mgr := NewManager(zerolog.New(&mgrLog))
	cloneURL := "https://user:" + marker + "@127.0.0.1:1/x.git?token=" + marker
	dest := filepath.Join(t.TempDir(), "clone")

	err := mgr.Clone(context.Background(), cloneURL, dest)
	if err == nil {
		t.Fatal("Clone of an unreachable loopback URL succeeded; want an error")
	}
	if strings.Contains(err.Error(), marker) {
		t.Fatalf("Clone error leaks the URL credential: %v", err)
	}
	if !strings.Contains(err.Error(), "127.0.0.1:1") {
		t.Fatalf("Clone error lost the redacted URL's host, want it kept for diagnosis: %v", err)
	}
	for name, out := range map[string]string{"manager": mgrLog.String(), "global": globalLog.String()} {
		if strings.Contains(out, marker) {
			t.Fatalf("%s log leaks the URL credential: %s", name, out)
		}
	}
	if !strings.Contains(mgrLog.String(), "cloning repository") {
		t.Fatalf("expected the clone log line to be captured, got: %s", mgrLog.String())
	}
}
