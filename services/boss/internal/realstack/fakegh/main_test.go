package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestFixtureResponses(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		args []string
		out  string
	}{
		{"open PRs", []string{"pr", "list", "--repo", "owner/repo", "--state", "open", "--json", "number,title,headRefName,state,author", "--limit", "300"}, `[{"number":42,"title":"Fix","headRefName":"fix","state":"OPEN","author":{"login":"dev"}}]`},
		{"closed PRs", []string{"pr", "list", "--repo", "owner/repo", "--state", "closed", "--json", "number,title,headRefName,state,author", "--limit", "50"}, `[]`},
		{"search PRs", []string{"pr", "list", "--repo", "owner/repo", "--state", "all", "--search", "TEST-1 in:title", "--json", "number,title,headRefName,state,author", "--limit", "100"}, `[{"number":42,"title":"[TEST-1] Fix"}]`},
		{"checks", []string{"pr", "checks", "42", "--repo", "owner/repo", "--json", "name,state,workflow,description"}, `[{"name":"test","state":"SUCCESS","workflow":"Go","description":"passed"}]`},
		{"PR status", []string{"pr", "view", "42", "--repo", "owner/repo", "--json", "state,mergeable,mergeStateStatus,isDraft,title,headRefName,baseRefName,headRefOid,isCrossRepository,reviewDecision,reviews"}, `{"state":"OPEN","mergeable":"MERGEABLE","mergeStateStatus":"CLEAN","reviewDecision":"APPROVED","reviews":[]}`},
		{"merge commit", []string{"pr", "view", "42", "--repo", "owner/repo", "--json", "state,mergeCommit"}, `{"state":"MERGED","mergeCommit":{"oid":"0123456789012345678901234567890123456789"}}`},
		{"approve", []string{"pr", "review", "42", "--repo", "owner/repo", "--approve"}, ""},
		{"merge", []string{"pr", "merge", "42", "--repo", "owner/repo", "--merge", "--delete-branch"}, ""},
		{"squash", []string{"pr", "merge", "42", "--repo", "owner/repo", "--squash", "--delete-branch"}, ""},
		{"rebase with pinned head", []string{"pr", "merge", "42", "--repo", "owner/repo", "--rebase", "--delete-branch", "--match-head-commit", "0123456789012345678901234567890123456789"}, ""},
		{"merge strategies", []string{"api", "repos/owner/repo", "--jq", "{m:.allow_merge_commit,s:.allow_squash_merge,r:.allow_rebase_merge}"}, `{"m":true,"s":true,"r":true}`},
		{"repo", []string{"repo", "view", "--json", "nameWithOwner"}, `{"nameWithOwner":"owner/repo"}`},
		{"auth", []string{"auth", "status"}, "Logged in to github.com as fixture\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			fixture := writeSpec(t, spec{Responses: []response{{Args: tc.args, Stdout: tc.out}}})
			var stdout, stderr bytes.Buffer
			if code := run(tc.args, fixture, &stdout, &stderr); code != 0 || stdout.String() != tc.out || stderr.Len() != 0 {
				t.Fatalf("run = (%d, %q, %q), want (0, %q, empty)", code, &stdout, &stderr, tc.out)
			}
			// Even one new flag is a new command contract, requiring its own fixture.
			if code := run(append(tc.args, "--unexpected"), fixture, &stdout, &stderr); code == 0 {
				t.Fatal("unfixtured flag succeeded")
			}
		})
	}
}

func TestFixtureFailureResponse(t *testing.T) {
	t.Parallel()
	args := []string{"pr", "checks", "42"}
	fixture := writeSpec(t, spec{Responses: []response{{Args: args, Stdout: "[]", Stderr: "no checks reported on the 'fix' branch", ExitCode: 8}}})
	var stdout, stderr bytes.Buffer
	if code := run(args, fixture, &stdout, &stderr); code != 8 || stdout.String() != "[]" || stderr.String() != "no checks reported on the 'fix' branch" {
		t.Fatalf("run = (%d, %q, %q)", code, &stdout, &stderr)
	}
}

func TestUnrecognisedArgvExitsNonzero(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	// Execute the production main function in a child so os.Exit is observed.
	cmd := exec.CommandContext(ctx, executable, "-test.run=^TestFakeGHEntrypoint$") // #nosec G204 -- current test executable
	cmd.Env = append(os.Environ(), "FAKE_GH_TEST_ENTRYPOINT=1", "BOSS_FAKE_GH_SPEC="+writeSpec(t, spec{}))
	cmd.WaitDelay = time.Second
	out, err := cmd.CombinedOutput()
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) || exitErr.ExitCode() == 0 {
		t.Fatalf("unknown argv process error = %v, output = %s", err, out)
	}
	if !strings.Contains(string(out), `unrecognised argv ["unexpected" "command"]`) {
		t.Fatalf("missing argv diagnostic: %s", out)
	}
}

func TestFakeGHEntrypoint(_ *testing.T) {
	if os.Getenv("FAKE_GH_TEST_ENTRYPOINT") != "1" {
		return
	}
	os.Args = []string{"gh", "unexpected", "command"}
	main()
}

func TestInvalidFixture(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		body string
	}{
		{"malformed JSON", "{"},
		{"negative exit", `{"responses":[{"args":["auth","status"],"exit_code":-1}]}`},
		{"overflow exit", `{"responses":[{"args":["auth","status"],"exit_code":256}]}`},
		{"duplicate", `{"responses":[{"args":["auth","status"]},{"args":["auth","status"]}]}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			path := filepath.Join(t.TempDir(), "gh.json")
			if err := os.WriteFile(path, []byte(tc.body), 0o600); err != nil {
				t.Fatal(err)
			}
			var stdout, stderr bytes.Buffer
			if code := run([]string{"auth", "status"}, path, &stdout, &stderr); code == 0 || stdout.Len() != 0 || stderr.Len() == 0 {
				t.Fatalf("invalid fixture = (%d, %q, %q)", code, &stdout, &stderr)
			}
		})
	}
}

func TestMissingFixture(t *testing.T) {
	t.Parallel()
	for _, path := range []string{"", filepath.Join(t.TempDir(), "missing.json")} {
		var stdout, stderr bytes.Buffer
		if code := run([]string{"auth", "status"}, path, &stdout, &stderr); code == 0 || stderr.Len() == 0 {
			t.Fatalf("missing fixture = (%d, %q)", code, &stderr)
		}
	}
}

func writeSpec(t *testing.T, fixture spec) string {
	t.Helper()
	data, err := json.Marshal(fixture)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "gh.json")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}
