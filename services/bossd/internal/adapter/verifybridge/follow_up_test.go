package verifybridge

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/recurser/bossd/internal/domain"
)

func TestFollowUpRunsSiblingWithSelection(t *testing.T) {
	f := newFixture(t, `{"ok":true,"head":"abc","comment":"updated","results":[{"note":"note:aaaaaaaaaaaa","outcome":"created","ticket":{"identifier":"T-1","url":"https://t/1"}},{"note":"note:bbbbbbbbbbbb","outcome":"already-filed","ticket":{"identifier":"T-2","url":"https://t/2"},"linked":false,"reason":"note-gone"}]}`, "", 0)
	f.bridge.Timeout = DefaultTimeout
	f.bridge.FollowUpScript = filepath.Join(filepath.Dir(f.bridge.Script), "verify-followups.mjs")
	result, err := f.bridge.FollowUp(context.Background(), Target{PR: 7, Repo: "o/r", Dir: f.worktree}, domain.VerifyFollowUp{VerifyNoteIDs: []string{"note:aaaaaaaaaaaa", "note:bbbbbbbbbbbb", "note:aaaaaaaaaaaa"}})
	if err != nil || !result.OK || len(result.Notes) != 2 || !result.Notes[0].IsLinked || result.Notes[1].IsLinked || result.Notes[0].Ticket.Identifier != "T-1" {
		t.Fatalf("FollowUp = %+v, %v", result, err)
	}
	want := []string{f.bridge.FollowUpScript, "follow-up", "--pr=7", "--repo=o/r", "--note=note:aaaaaaaaaaaa", "--note=note:bbbbbbbbbbbb", "--json"}
	if got := f.argv(t); !reflect.DeepEqual(got, want) {
		t.Fatalf("argv = %q; want %q", got, want)
	}
	if got := f.cwd(t); got != resolved(t, f.worktree) {
		t.Fatalf("cwd = %q", got)
	}
}

func TestFollowUpResultsAndFailureModes(t *testing.T) {
	for _, tc := range []struct {
		name, out string
		code      int
		dry       bool
		want      error
	}{
		{name: "unknown note refusal", out: `{"ok":false,"reason":"unknown-note","ids":["note:aaaaaaaaaaaa"]}`, code: 1},
		{name: "partial", out: `{"ok":false,"comment":"updated","results":[{"note":"note:aaaaaaaaaaaa","outcome":"indeterminate"},{"note":"note:bbbbbbbbbbbb","outcome":"failed","reason":"tracker-error"}]}`, code: 1},
		{name: "failed comment", out: `{"ok":false,"comment":"failed","results":[]}`, code: 1},
		{name: "dry run with existing ticket", out: `{"ok":true,"dryRun":true,"comment":"planned","results":[{"note":"note:aaaaaaaaaaaa","outcome":"already-filed","ticket":{"identifier":"T-1","url":"https://t/1"}},{"note":"note:bbbbbbbbbbbb","outcome":"planned"}]}`, dry: true},
		{name: "usage", out: `{"ok":false,"reason":"usage"}`, code: 2, want: ErrNodeExit},
		{name: "empty", code: 1, want: ErrEmptyOutput},
		{name: "non json", out: "oops", code: 1, want: ErrBadJSON},
		{name: "missing ok", out: `{"reason":"no"}`, want: ErrShape},
		{name: "exit zero refusal", out: `{"ok":false,"reason":"no"}`, want: ErrShape},
		{name: "exit one success", out: `{"ok":true,"comment":"current","results":[]}`, code: 1, want: ErrShape},
		{name: "unknown outcome", out: `{"ok":true,"comment":"current","results":[{"note":"note:aaaaaaaaaaaa","outcome":"other"}]}`, want: ErrShape},
		{name: "unknown comment", out: `{"ok":true,"comment":"other","results":[]}`, want: ErrShape},
		{name: "ticket missing", out: `{"ok":true,"comment":"updated","results":[{"note":"note:aaaaaaaaaaaa","outcome":"created"}]}`, want: ErrShape},
		{name: "refusal without cause", out: `{"ok":false}`, code: 1, want: ErrShape},
		{name: "failure on success", out: `{"ok":true,"comment":"current","results":[{"note":"note:aaaaaaaaaaaa","outcome":"failed"}]}`, want: ErrShape},
		{name: "planned outside dry run", out: `{"ok":true,"comment":"planned","results":[]}`, want: ErrShape},
		{name: "missing results on success", out: `{"ok":true,"comment":"current"}`, want: ErrShape},
		{name: "null note", out: `{"ok":true,"comment":"current","results":[null]}`, want: ErrShape},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t, tc.out, "", tc.code)
			f.bridge.Timeout = DefaultTimeout
			f.bridge.FollowUpScript = f.bridge.Script
			got, err := f.bridge.FollowUp(context.Background(), Target{PR: 7, Dir: f.worktree, DryRun: tc.dry}, domain.VerifyFollowUp{All: true})
			if !errors.Is(err, tc.want) || (tc.want == nil && err != nil) {
				t.Fatalf("result=%+v err=%v want=%v", got, err, tc.want)
			}
			if tc.name == "unknown note refusal" && !reflect.DeepEqual(got.UnknownVerifyNoteIDs, []string{"note:aaaaaaaaaaaa"}) {
				t.Fatalf("unknown ids=%v", got.UnknownVerifyNoteIDs)
			}
			if tc.want == nil {
				args := f.argv(t)
				if !strings.Contains(strings.Join(args, " "), "--all") {
					t.Fatalf("argv=%v", args)
				}
			}
		})
	}
}

func TestFollowUpRejectsSelectionBeforeRunning(t *testing.T) {
	for _, s := range []domain.VerifyFollowUp{{}, {All: true, VerifyNoteIDs: []string{"note:aaaaaaaaaaaa"}}, {VerifyNoteIDs: []string{"bad"}}} {
		f := newFixture(t, "", "", 0)
		f.bridge.Timeout = DefaultTimeout
		f.bridge.FollowUpScript = f.bridge.Script
		if _, err := f.bridge.FollowUp(context.Background(), Target{PR: 7, Dir: f.worktree}, s); !errors.Is(err, ErrInvalidTarget) {
			t.Fatalf("err=%v", err)
		}
		if _, err := os.Stat(f.argvFile); err == nil {
			t.Fatal("node ran")
		}
	}
}

func TestFollowUpCommentFailureDoesNotConfirmLinks(t *testing.T) {
	for _, tc := range []struct {
		name, output, reason string
	}{
		{name: "created before reread failed", output: `{"ok":false,"comment":"failed","reason":"comments-unreadable","results":[{"note":"note:aaaaaaaaaaaa","outcome":"created","ticket":{"identifier":"T-1","url":"https://t/1"}}]}`, reason: "comments-unreadable"},
		{name: "created before patch failed", output: `{"ok":false,"comment":"failed","results":[{"note":"note:aaaaaaaaaaaa","outcome":"created","ticket":{"identifier":"T-1","url":"https://t/1"}}]}`, reason: "comment-update-failed"},
		{name: "already filed before patch failed", output: `{"ok":false,"comment":"failed","results":[{"note":"note:aaaaaaaaaaaa","outcome":"already-filed","ticket":{"identifier":"T-1","url":"https://t/1"}}]}`, reason: "comment-update-failed"},
		{name: "explicit unlinked reason survives", output: `{"ok":false,"comment":"failed","results":[{"note":"note:aaaaaaaaaaaa","outcome":"created","ticket":{"identifier":"T-1","url":"https://t/1"},"linked":false,"reason":"note-gone"}]}`, reason: "note-gone"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t, tc.output, "", 1)
			f.bridge.Timeout = DefaultTimeout
			f.bridge.FollowUpScript = f.bridge.Script
			got, err := f.bridge.FollowUp(t.Context(), Target{PR: 7, Dir: f.worktree}, domain.VerifyFollowUp{All: true})
			if err != nil || got.OK || got.Comment != "failed" || len(got.Notes) != 1 {
				t.Fatalf("FollowUp = %+v, %v", got, err)
			}
			note := got.Notes[0]
			if note.IsLinked || note.Reason != tc.reason || note.Ticket == nil || note.Ticket.Identifier != "T-1" || note.Ticket.URL != "https://t/1" {
				t.Fatalf("filed ticket linkage = %+v", note)
			}
		})
	}
}

func TestFollowUpMissingSiblingHasInstallHint_Sessions_AC213804(t *testing.T) {
	f := newFixture(t, "", "", 0)
	t.Setenv("BOSS_VERIFY_TOOLBOX", filepath.Dir(filepath.Dir(f.bridge.Script)))
	t.Setenv("BOSS_SKILLS_HOME", "")
	t.Setenv("HOME", t.TempDir())
	_, err := f.bridge.FollowUp(context.Background(), Target{PR: 7, Dir: f.worktree}, domain.VerifyFollowUp{All: true})
	if !errors.Is(err, ErrToolboxMissing) || !strings.Contains(err.Error(), "boss skills install") {
		t.Fatalf("err=%v", err)
	}
	if _, err := os.Stat(f.argvFile); err == nil {
		t.Fatal("node ran")
	}
}

func TestRealNodeRunsFollowUpExitOneResult(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not available")
	}
	dir := t.TempDir()
	script := filepath.Join(dir, "verify-followups.mjs")
	body := `const [verb,...args]=process.argv.slice(2)
if (verb !== 'follow-up' || !args.includes('--all') || !args.includes('--json')) process.exit(2)
console.log(JSON.stringify({ok:false,reason:'unknown-note',ids:['note:aaaaaaaaaaaa']}))
process.exitCode=1
`
	if err := os.WriteFile(script, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	bridge := Bridge{Node: node, FollowUpScript: script}
	result, err := bridge.FollowUp(context.Background(), Target{PR: 7, Dir: dir}, domain.VerifyFollowUp{All: true})
	if err != nil || result.OK || result.Reason != "unknown-note" || len(result.UnknownVerifyNoteIDs) != 1 {
		t.Fatalf("result=%+v err=%v", result, err)
	}
}
