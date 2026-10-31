package verifyadmission

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestAdmissionLookup(t *testing.T) {
	head := strings.Repeat("a", 40)
	for _, tc := range []struct {
		name   string
		states []string
		want   Answer
	}{
		{"admitted", []string{"admitted"}, Answer{StateAdmitted, "reason"}},
		{"pending", []string{"pending"}, Answer{StatePending, "reason"}},
		{"excluded", []string{"excluded"}, Answer{StateExcluded, "reason"}},
		{"all excluded", []string{"excluded", "excluded"}, Answer{StateExcluded, "reason"}},
		{"admitted wins", []string{"excluded", "pending", "admitted"}, Answer{StateAdmitted, "reason"}},
		{"pending wins", []string{"excluded", "pending"}, Answer{StatePending, "reason"}},
		{"unknown", []string{"future"}, Answer{}},
		{"unknown vetoes excluded", []string{"future", "excluded"}, Answer{}},
		{"admitted beats unknown", []string{"future", "admitted"}, Answer{StateAdmitted, "reason"}},
		{"pending beats unknown", []string{"future", "pending"}, Answer{StatePending, "reason"}},
		{"empty", nil, Answer{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			snapshot := Snapshot{Version: 1, Scopes: map[string]scope{}}
			for i, state := range tc.states {
				snapshot.Scopes[strconv.Itoa(i)] = scope{PRs: map[string]entry{"3550": {head, state, "reason"}}}
			}
			if got := snapshot.Lookup(3550, strings.ToUpper(head)); got != tc.want {
				t.Fatalf("Lookup=%+v want %+v", got, tc.want)
			}
			for _, pr := range []int{0, -1, 9999} {
				if got := snapshot.Lookup(pr, head); got != (Answer{}) {
					t.Fatalf("invalid/absent PR: %+v", got)
				}
			}
			for _, queryHead := range []string{"", "old"} {
				if got := snapshot.Lookup(3550, queryHead); got != (Answer{}) {
					t.Fatalf("missing head: %+v", got)
				}
			}
			snapshot.Version = 2
			if got := snapshot.Lookup(3550, head); got != (Answer{}) {
				t.Fatalf("unsupported version: %+v", got)
			}
		})
	}
}

func TestAdmissionLookupReasonOrder(t *testing.T) {
	head := strings.Repeat("a", 40)
	for _, state := range []string{StateAdmitted, StatePending, StateExcluded} {
		snapshot := Snapshot{Version: 1, Scopes: map[string]scope{
			"one": {PRs: map[string]entry{"3550": {head, state, "z"}}},
			"two": {PRs: map[string]entry{"3550": {head, state, "a"}}},
			"old": {PRs: map[string]entry{"3550": {"old", "admitted", ""}}},
		}}
		for range 20 {
			if got := snapshot.Lookup(3550, head); got != (Answer{state, "a"}) {
				t.Fatalf("got %+v", got)
			}
		}
		snapshot.Scopes["empty"] = scope{PRs: map[string]entry{"3550": {head, state, ""}}}
		if got := snapshot.Lookup(3550, head); got != (Answer{State: state}) {
			t.Fatalf("empty reason: %+v", got)
		}
	}
}

func TestAdmissionLookupDecidedPendingReason(t *testing.T) {
	head := strings.Repeat("a", 40)
	for _, reason := range []string{"ticket-not-selected", "ticket-needs-person", "z"} {
		t.Run(reason, func(t *testing.T) {
			snapshot := Snapshot{Version: 1, Scopes: map[string]scope{
				"unprobed": {PRs: map[string]entry{"3550": {head, StatePending, "ticket-not-in-review"}}},
				"decided":  {PRs: map[string]entry{"3550": {head, StatePending, reason}}},
			}}
			for range 20 {
				if got := snapshot.Lookup(3550, head); got != (Answer{StatePending, reason}) {
					t.Fatalf("Lookup=%+v want decided pending reason %q", got, reason)
				}
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
		if got := snapshot.Lookup(3550, strings.Repeat("a", 40)); got != (Answer{}) {
			t.Fatalf("invalid %q excluded", content)
		}
	}
	for _, path := range []string{filepath.Join(t.TempDir(), "missing"), t.TempDir()} {
		snapshot, _ := Read(path)
		if got := snapshot.Lookup(3550, strings.Repeat("a", 40)); got != (Answer{}) {
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
		want Answer
	}{
		{3550, "a", Answer{StateExcluded, "no-ticket"}},
		{3551, "b", Answer{StateAdmitted, "ticket"}},
		{3552, "c", Answer{StatePending, "ticket-not-in-review"}},
		{3553, "d", Answer{StateExcluded, "no-ticket"}},
		{3550, "b", Answer{}}, {9999, "a", Answer{}},
	} {
		if got := snapshot.Lookup(tc.pr, strings.Repeat(tc.head, 40)); got != tc.want {
			t.Errorf("PR%d got %+v want %+v", tc.pr, got, tc.want)
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
