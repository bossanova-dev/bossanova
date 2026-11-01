package verifybridge

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/recurser/bossd/internal/domain"
)

func TestAcceptResultsAndShape(t *testing.T) {
	head := strings.Repeat("a", 40)
	success := `"accepted":true,"head":"` + head + `","blocker":"review:0123456789ab","ticket":{"identifier":"TEST-1","url":"https://tracker/1"},"commentUrl":"https://comment/1"`
	for _, tc := range []struct {
		name, out string
		dry, bad  bool
	}{
		{"success", `{` + success + `}`, false, false},
		{"existing ticket", `{` + success + `,"existingTicket":true}`, false, false},
		{"already", `{"accepted":true,"already":true,"head":"` + head + `","blocker":"review:0123456789ab","ticket":{"identifier":"TEST-1"},"commentUrl":"https://comment/1"}`, false, false},
		{"refusal", `{"accepted":false,"reason":"not-open","detail":"closed"}`, false, false},
		{"dry run", `{"accepted":false,"dryRun":true,"writes":["read",{"kind":"tracker-issue"}]}`, true, false},
		{"empty writes", `{"accepted":false,"dryRun":true,"writes":[]}`, true, false},
		{"missing accepted", `{}`, false, true},
		{"head moved", `{` + strings.Replace(success, head, strings.Repeat("b", 40), 1) + `}`, false, true},
		{"wrong blocker", `{` + strings.Replace(success, "review:0123456789ab", "review:bbbbbbbbbbbb", 1) + `}`, false, true},
		{"missing identifier", `{` + strings.Replace(success, `"identifier":"TEST-1",`, "", 1) + `}`, false, true},
		{"missing ticket URL", `{` + strings.Replace(success, `,"url":"https://tracker/1"`, "", 1) + `}`, false, true},
		{"missing comment", `{` + strings.Replace(success, `,"commentUrl":"https://comment/1"`, "", 1) + `}`, false, true},
		{"unexpected dry run", `{"accepted":false,"dryRun":true,"writes":[]}`, false, true},
		{"missing writes", `{"accepted":false,"dryRun":true}`, true, true},
		{"nonarray writes", `{"accepted":false,"dryRun":true,"writes":"oops"}`, true, true},
		{"null writes", `{"accepted":false,"dryRun":true,"writes":null}`, true, true},
		{"missing refusal reason", `{"accepted":false}`, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t, tc.out, "", 0)
			r, err := f.bridge.Accept(context.Background(), Target{PR: 7, Dir: f.worktree, DryRun: tc.dry}, domain.VerifyAccept{Head: head, Blocker: "review:0123456789ab", Reason: "keep literal"})
			if errors.Is(err, ErrShape) != tc.bad || !tc.bad && err != nil {
				t.Fatalf("result=%+v error=%v", r, err)
			}
			if tc.name == "dry run" && !reflect.DeepEqual(r.Writes, []string{"read", `{"kind":"tracker-issue"}`}) {
				t.Fatalf("writes=%v", r.Writes)
			}
			if tc.name == "existing ticket" && !r.ExistingTicket {
				t.Fatal("lost existing ticket")
			}
			if tc.name == "refusal" && (r.Reason != "not-open" || r.Detail != "closed") {
				t.Fatalf("refusal=%+v", r)
			}
		})
	}
}

func TestAcceptPrivateReasonAndArgv(t *testing.T) {
	f := newFixture(t, `{"accepted":false,"reason":"not-open"}`, "", 0)
	f.bridge.Timeout = 30 * time.Second
	copyPath := filepath.Join(t.TempDir(), "copy")
	addProbe(t, f, `for a in "$@"; do case "$a" in --reason-file=*) p=${a#--reason-file=}; test "$(ls -l "$p" | cut -c 1-10)" = '-rw-------' || exit 8; cat "$p" > '`+copyPath+`';; esac; done`)
	reason := "reason `$(literal)`\n<!-- @user -->"
	head := strings.Repeat("a", 40)
	_, err := f.bridge.Accept(context.Background(), Target{PR: 7, Repo: "owner/repo", Dir: f.worktree, DryRun: true}, domain.VerifyAccept{Head: head, Blocker: "review:0123456789ab", Reason: reason})
	if err != nil {
		t.Fatal(err)
	}
	args := f.argv(t)
	want := []string{f.bridge.Script, "accept", "--pr=7", "--repo=owner/repo", "--dry-run", "--head=" + head, "--blocker=review:0123456789ab", args[len(args)-2], "--json"}
	if !reflect.DeepEqual(args, want) || !strings.HasPrefix(args[len(args)-2], "--reason-file=") {
		t.Fatalf("argv=%v", args)
	}
	path := strings.TrimPrefix(args[len(args)-2], "--reason-file=")
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("reason file remains: %v", err)
	}
	if got := readFile(t, copyPath); got != reason {
		t.Fatalf("reason=%q", got)
	}
}

func addProbe(t *testing.T, f *fixture, probe string) {
	t.Helper()
	body, err := os.ReadFile(f.bridge.Node)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(f.bridge.Node, []byte(strings.Replace(string(body), "#!/bin/sh\n", "#!/bin/sh\n"+probe+"\n", 1)), 0o755); err != nil {
		t.Fatal(err)
	}
}

func TestBlockersOutcomesAndShape(t *testing.T) {
	head := strings.Repeat("a", 40)
	file := `{"version":1,"pr":7,"head":"` + head + `","blockers":[{"id":"review:0123456789ab","kind":"review-finding","severity":"P1","file":"a.go","line":12,"summary":"bug","text":"details"}]}`
	for _, tc := range []struct {
		name, out, body string
		code            int
		want            error
	}{
		{"written", `{"outcome":"written"}`, file, 0, nil},
		{"absent", `{"outcome":"absent"}`, "", 3, nil},
		{"unreadable", `{"outcome":"unreadable"}`, "", 1, nil},
		{"non-JSON failure", "failure", "", 1, ErrNodeExit},
		{"no output", "", "", 1, ErrNodeExit},
		{"unallowed exit", `{"outcome":"absent"}`, "", 2, ErrNodeExit},
		{"wrong outcome", `{"outcome":"absent"}`, "", 0, ErrShape},
		{"wrong exit", `{"outcome":"written"}`, file, 3, ErrShape},
		{"wrong PR", `{"outcome":"written"}`, strings.Replace(file, `"pr":7`, `"pr":8`, 1), 0, ErrShape},
		{"wrong head", `{"outcome":"written"}`, strings.Replace(file, head, strings.Repeat("b", 40), 1), 0, ErrShape},
		{"bad head", `{"outcome":"written"}`, strings.Replace(file, head, "bad", 1), 0, ErrShape},
		{"bad version", `{"outcome":"written"}`, strings.Replace(file, `"version":1`, `"version":2`, 1), 0, ErrShape},
		{"bad id", `{"outcome":"written"}`, strings.Replace(file, "review:0123456789ab", "review:bad", 1), 0, ErrShape},
		{"missing id", `{"outcome":"written"}`, strings.Replace(file, "review:0123456789ab", "", 1), 0, ErrShape},
		{"unknown kind", `{"outcome":"written"}`, strings.Replace(file, "review-finding", "new-kind", 1), 0, ErrShape},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t, tc.out, "", tc.code)
			source := filepath.Join(t.TempDir(), "blockers.json")
			if err := os.WriteFile(source, []byte(tc.body), 0o600); err != nil {
				t.Fatal(err)
			}
			addProbe(t, f, `for a in "$@"; do case "$a" in --out=*) cp '`+source+`' "${a#--out=}";; esac; done`)
			r, err := f.bridge.Blockers(context.Background(), Target{PR: 7, Dir: f.worktree, DryRun: true}, head)
			if tc.want != nil && !errors.Is(err, tc.want) || tc.want == nil && err != nil {
				t.Fatalf("result=%+v error=%v want=%v", r, err, tc.want)
			}
			if tc.name == "written" && (r.Head != head || len(r.Blockers) != 1 || r.Blockers[0].Line != 12) {
				t.Fatalf("result=%+v", r)
			}
			for _, a := range f.argv(t) {
				if a == "--json" || a == "--dry-run" {
					t.Fatalf("unexpected flag %s", a)
				}
				if strings.HasPrefix(a, "--out=") {
					if _, err := os.Stat(filepath.Dir(strings.TrimPrefix(a, "--out="))); !errors.Is(err, os.ErrNotExist) {
						t.Fatalf("directory remains: %v", err)
					}
				}
			}
		})
	}
}

func TestAcceptInvalidPin(t *testing.T) {
	for _, pin := range []domain.VerifyAccept{{Head: "bad", Blocker: "id"}, {Head: strings.Repeat("a", 40)}} {
		f := newFixture(t, "{}", "", 0)
		if _, err := f.bridge.Accept(context.Background(), Target{PR: 7, Dir: f.worktree}, pin); !errors.Is(err, ErrInvalidTarget) {
			t.Fatalf("error=%v", err)
		}
		if _, err := os.Stat(f.argvFile); !errors.Is(err, os.ErrNotExist) {
			t.Fatal("ran invalid pin")
		}
	}
}

func TestBlockersDuplicateIDs(t *testing.T) {
	head := strings.Repeat("a", 40)
	blocker := domain.VerifyBlocker{ID: "review:0123456789ab", Kind: "review-finding"}
	payload, _ := json.Marshal(map[string]any{"version": 1, "pr": 7, "head": head, "blockers": []domain.VerifyBlocker{blocker, blocker}})
	f := newFixture(t, `{"outcome":"written"}`, "", 0)
	source := filepath.Join(t.TempDir(), "source")
	if err := os.WriteFile(source, payload, 0o600); err != nil {
		t.Fatal(err)
	}
	addProbe(t, f, `for a in "$@"; do case "$a" in --out=*) cp '`+source+`' "${a#--out=}";; esac; done`)
	if _, err := f.bridge.Blockers(context.Background(), Target{PR: 7, Dir: f.worktree}, head); !errors.Is(err, ErrShape) {
		t.Fatalf("error=%v", err)
	}
}

func TestAcceptNonzeroExitStillFailsAndRemovesReason(t *testing.T) {
	for _, code := range []int{1, 3} {
		t.Run(string(rune('0'+code)), func(t *testing.T) {
			f := newFixture(t, `{"accepted":false,"reason":"not-open"}`, "", code)
			_, err := f.bridge.Accept(context.Background(), Target{PR: 7, Dir: f.worktree}, domain.VerifyAccept{Head: strings.Repeat("a", 40), Blocker: "review:0123456789ab", Reason: "literal"})
			if !errors.Is(err, ErrNodeExit) {
				t.Fatalf("error=%v", err)
			}
			for _, arg := range f.argv(t) {
				if strings.HasPrefix(arg, "--reason-file=") {
					if _, err := os.Stat(strings.TrimPrefix(arg, "--reason-file=")); !errors.Is(err, os.ErrNotExist) {
						t.Fatalf("file remains: %v", err)
					}
				}
			}
		})
	}
}

func TestBlockersAllKindsWithoutHeadPin(t *testing.T) {
	head := strings.Repeat("A", 40)
	payload := `{"version":1,"pr":7,"head":"` + head + `","blockers":[{"id":"operator-note","kind":"operator-note","severity":null,"file":null,"line":null,"summary":"note","text":"literal"},{"id":"review:0123456789ab","kind":"review-finding","severity":"P1","file":null,"line":null,"summary":"bug","text":"details"},{"id":"check:build","kind":"check","severity":null,"file":null,"line":null,"summary":"build","text":"failed"},{"id":"conflict","kind":"conflict","severity":null,"file":null,"line":null,"summary":"conflict","text":"conflict"},{"id":"no-review","kind":"no-review","severity":null,"file":null,"line":null,"summary":"no review","text":"no review"}]}`
	source := filepath.Join(t.TempDir(), "source")
	if err := os.WriteFile(source, []byte(payload), 0o600); err != nil {
		t.Fatal(err)
	}
	f := newFixture(t, `{"outcome":"written"}`, "", 0)
	addProbe(t, f, `for a in "$@"; do case "$a" in --out=*) cp '`+source+`' "${a#--out=}";; esac; done`)
	r, err := f.bridge.Blockers(context.Background(), Target{PR: 7, Dir: f.worktree}, "")
	if err != nil || r.Head != head || len(r.Blockers) != 5 {
		t.Fatalf("result=%+v error=%v", r, err)
	}
	for _, arg := range f.argv(t) {
		if strings.HasPrefix(arg, "--head=") {
			t.Fatalf("unexpected head pin: %s", arg)
		}
	}
}
