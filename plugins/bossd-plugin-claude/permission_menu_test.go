package main

import (
	"strings"
	"testing"

	bossanovav1 "github.com/recurser/bossalib/gen/bossanova/v1"
	"google.golang.org/protobuf/proto"
)

// Real captured panes from Claude Code 2.1.330 via `tmux capture-pane -p -S -1000`.

const permSubagentBash2Row = `⏺ Re-arm hold for drafting dispatch · 5m 46s
  ⎿  $ ls -la .linear-plans/run-ldg8pbcP/

────────────────────────────────────────────────────────────────────────────────
Bash command · from the general-purpose agent
Scratch-test canned recipe idiom on make 3.81
────────────────────────────────────────────────────────────────────────────────
  │ mkdir -p /tmp/fre2636mk/perf.d && cd /tmp/fre2636mk && cat > perf.d/20-b.mk <<'EOF'
  │ # gate b
  │ PERF_B_X ?= 7
  │ EOF
  │ make -n t; echo "exit=$?"
────────────────────────────────────────────────────────────────────────────────
  │ Claude Code can't be sure it read this command the way the shell will, so it can't rule out a dangerous removal. Read the command, and don't go
  │ ahead if you can't tell what it does.

Do you want to proceed?
❯ 1. Yes
  2. No

Esc to cancel · Tab to amend · ctrl+x ctrl+k twice to stop background agents
`

const permBashWarning3Row = `❯ Run this exact bash command: mkdir -p /tmp/permcap_x && rm -rf /tmp/permcap_x

⏺ Creating temp dir and writing a script file via heredoc
  ⎿  $ mkdir -p /tmp/permcap_x && cd /tmp/permcap_x && cat > a.sh <<'EOF'echo hiEOFrm -rf
     /tmp/permcap_x

────────────────────────────────────────────────────────────────────────────────────────────────────
 Bash command
 Tip: auto mode handles these prompts for you — choose "switch to auto mode" below
 Create temp dir and write a script file via heredoc
╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌
 │ mkdir -p /tmp/permcap_x && cd /tmp/permcap_x && cat > a.sh <<'EOF'echo hiEOFrm -rf
 │ /tmp/permcap_x
╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌
 Parser aborted (timeout, resource limit, or over-length)

 Do you want to proceed?
 ❯ 1. Yes
   2. Yes, and switch to auto mode · auto mode handles these prompts for you
   3. No

 Esc to cancel · Tab to amend
`

const permBash4Row = `❯ Run the bash command: touch perm_test.txt && echo hi > perm_test.txt

⏺ Creating test file and writing text to it
  ⎿  $ touch perm_test.txt && echo hi > perm_test.txt

────────────────────────────────────────────────────────────────────────────────────────────────────
 Bash command
 Tip: auto mode handles these prompts for you — choose "switch to auto mode" below
 Create test file and write text to it
╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌
 touch perm_test.txt && echo hi > perm_test.txt
╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌
 Do you want to proceed?
 ❯ 1. Yes
   2. Yes, and don't ask again for touch perm_test.txt and echo hi commands in
      /private/tmp/boss-perm-capture.EBwUO6
   3. Yes, and switch to auto mode · auto mode handles these prompts for you
   4. No

 Esc to cancel · Tab to amend
`

const permEditFile3Row = `❯ Use the Edit tool to change hello to world in a.txt

  Read 1 file

⏺ Update(a.txt)

────────────────────────────────────────────────────────────────────────────────────────────────────
 Edit file
 a.txt
╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌
 1 -hello
 1 +world
╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌
 Do you want to make this edit to a.txt?
 ❯ 1. Yes
   2. Yes, and switch to accept edits (auto-approve file edits and common file commands) for this
      session (shift+tab)
   3. No

 Esc to cancel · Tab to amend
`

const permTruncatedTop = `  │   echo line 999
  │   echo line 1000
  │ EOF
────────────────────────────────────────────────────────────────────────────────
  │ Claude Code can't be sure it read this command the way the shell will.

Do you want to proceed?
❯ 1. Yes
  2. No

Esc to cancel · Tab to amend
`

const permAmendedRow = `────────────────────────────────────────────────────────────────────────────────────────────────────
 Bash command
 Create temp dir
╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌
 mkdir -p /tmp/permcap_x
╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌
 Do you want to proceed?
 ❯ 1. Yes, and tell Claude what to do next
   2. No

 Esc to cancel
`

func readMenu(t *testing.T, pane string) *bossanovav1.ChatQuestion {
	t.Helper()
	resp, err := (&Server{}).ReadQuestionMenu(t.Context(), &bossanovav1.ReadQuestionMenuRequest{
		PaneContent: []byte(pane),
	})
	if err != nil {
		t.Fatalf("ReadQuestionMenu: %v", err)
	}
	return resp.GetQuestion()
}

func TestReadQuestionMenu_ReadsPermissionDialogs(t *testing.T) {
	t.Run("subagent 2-row bash with warning and gutter", func(t *testing.T) {
		q := readMenu(t, permSubagentBash2Row)
		if q == nil {
			t.Fatal("expected permission question, got nil")
		}
		if q.GetKind() != "permission" {
			t.Fatalf("kind = %q, want permission", q.GetKind())
		}
		wantTitle := "Bash command · from the general-purpose agent · Scratch-test canned recipe idiom on make 3.81"
		if q.GetTitle() != wantTitle {
			t.Fatalf("title = %q, want %q", q.GetTitle(), wantTitle)
		}
		wantDetail := "mkdir -p /tmp/fre2636mk/perf.d && cd /tmp/fre2636mk && cat > perf.d/20-b.mk <<'EOF'\n# gate b\nPERF_B_X ?= 7\nEOF\nmake -n t; echo \"exit=$?\""
		if q.GetDetail() != wantDetail {
			t.Fatalf("detail = %q, want %q", q.GetDetail(), wantDetail)
		}
		wantWarn := "Claude Code can't be sure it read this command the way the shell will, so it can't rule out a dangerous removal. Read the command, and don't go ahead if you can't tell what it does."
		if q.GetWarning() != wantWarn {
			t.Fatalf("warning = %q, want %q", q.GetWarning(), wantWarn)
		}
		if q.GetText() != "Do you want to proceed?" {
			t.Fatalf("text = %q, want Do you want to proceed?", q.GetText())
		}
		if q.GetIsDetailTruncated() {
			t.Fatal("is_detail_truncated = true, want false")
		}
		if !q.GetIsAnswerable() {
			t.Fatal("is_answerable = false, want true")
		}
		if got := strings.Join(q.GetOptions(), "|"); got != "Yes|No" {
			t.Fatalf("options = %q, want Yes|No", got)
		}
		if !strings.HasPrefix(q.GetId(), "perm-") {
			t.Fatalf("id = %q, want perm- prefix", q.GetId())
		}
	})

	t.Run("3-row bash with parser warning", func(t *testing.T) {
		q := readMenu(t, permBashWarning3Row)
		if q == nil {
			t.Fatal("expected permission question, got nil")
		}
		if q.GetTitle() != "Bash command · Create temp dir and write a script file via heredoc" {
			t.Fatalf("title = %q", q.GetTitle())
		}
		if q.GetWarning() != "Parser aborted (timeout, resource limit, or over-length)" {
			t.Fatalf("warning = %q", q.GetWarning())
		}
		if len(q.GetOptions()) != 3 || q.GetOptions()[0] != "Yes" || q.GetOptions()[2] != "No" {
			t.Fatalf("options = %#v", q.GetOptions())
		}
	})

	t.Run("4-row bash with wrapped option and tip line stripped from title", func(t *testing.T) {
		q := readMenu(t, permBash4Row)
		if q == nil {
			t.Fatal("expected permission question, got nil")
		}
		if q.GetTitle() != "Bash command · Create test file and write text to it" {
			t.Fatalf("title = %q", q.GetTitle())
		}
		if q.GetDetail() != "touch perm_test.txt && echo hi > perm_test.txt" {
			t.Fatalf("detail = %q", q.GetDetail())
		}
		if q.GetWarning() != "" {
			t.Fatalf("warning = %q, want empty", q.GetWarning())
		}
		wantOpt2 := "Yes, and don't ask again for touch perm_test.txt and echo hi commands in /private/tmp/boss-perm-capture.EBwUO6"
		if len(q.GetOptions()) != 4 || q.GetOptions()[1] != wantOpt2 {
			t.Fatalf("options = %#v", q.GetOptions())
		}
	})

	t.Run("non-bash edit file dialog with diff", func(t *testing.T) {
		q := readMenu(t, permEditFile3Row)
		if q == nil {
			t.Fatal("expected permission question, got nil")
		}
		if q.GetTitle() != "Edit file · a.txt" {
			t.Fatalf("title = %q", q.GetTitle())
		}
		if q.GetDetail() != "1 -hello\n1 +world" {
			t.Fatalf("detail = %q", q.GetDetail())
		}
		if q.GetText() != "Do you want to make this edit to a.txt?" {
			t.Fatalf("text = %q", q.GetText())
		}
		if len(q.GetOptions()) != 3 {
			t.Fatalf("options = %#v", q.GetOptions())
		}
	})

	t.Run("command scrolled off the top marks is_detail_truncated", func(t *testing.T) {
		q := readMenu(t, permTruncatedTop)
		if q == nil {
			t.Fatal("expected permission question, got nil")
		}
		if !q.GetIsDetailTruncated() {
			t.Fatal("is_detail_truncated = false, want true")
		}
		if q.GetTitle() != "" {
			t.Fatalf("title = %q, want empty when header scrolled off", q.GetTitle())
		}
		if !strings.Contains(q.GetDetail(), "echo line 1000") {
			t.Fatalf("detail = %q, want visible tail of command", q.GetDetail())
		}
		if q.GetWarning() != "Claude Code can't be sure it read this command the way the shell will." {
			t.Fatalf("warning = %q", q.GetWarning())
		}
	})

	t.Run("trust-folder dialog with unnumbered rows is not read as a permission dialog", func(t *testing.T) {
		trust := "Quick safety check: Is this a project you created?\n❯ No, exit\n  Yes, I trust this folder\n\nEnter to confirm · Esc to cancel\n"
		if q := readMenu(t, trust); q != nil {
			t.Fatalf("got %v, want nil", q)
		}
	})
}

func TestPlanQuestionAnswer_PermissionDialog(t *testing.T) {
	q := readMenu(t, permSubagentBash2Row)
	if q == nil {
		t.Fatal("expected permission question")
	}

	t.Run("selects row by digit", func(t *testing.T) {
		got := plan(t, planRequest(q.GetId(), permSubagentBash2Row, option(1)))
		want := &bossanovav1.PlanQuestionAnswerResponse{
			IsQuestionPending: true,
			Inputs:            []*bossanovav1.PaneInput{key("2")},
		}
		if !proto.Equal(got, want) {
			t.Fatalf("plan = %v, want %v", got, want)
		}
	})

	t.Run("refuses when the command changed", func(t *testing.T) {
		changed := strings.Replace(permSubagentBash2Row, "PERF_B_X ?= 7", "rm -rf /", 1)
		got := plan(t, planRequest(q.GetId(), changed, option(0)))
		if got.GetIsQuestionPending() || len(got.GetInputs()) != 0 {
			t.Fatalf("plan = %v, want not pending with no inputs", got)
		}
	})

	t.Run("refuses typed text on permission dialog", func(t *testing.T) {
		got := plan(t, planRequest(q.GetId(), permSubagentBash2Row, words("yes please")))
		if !got.GetIsQuestionPending() || len(got.GetInputs()) != 0 || got.GetReason() == "" {
			t.Fatalf("plan = %v, want pending refusal with reason", got)
		}
	})

	t.Run("refuses out-of-range option", func(t *testing.T) {
		got := plan(t, planRequest(q.GetId(), permSubagentBash2Row, option(2)))
		if !got.GetIsQuestionPending() || len(got.GetInputs()) != 0 || got.GetReason() == "" {
			t.Fatalf("plan = %v, want pending refusal with reason", got)
		}
	})

	t.Run("refuses amend-mode row that waits for terminal input", func(t *testing.T) {
		amended := readMenu(t, permAmendedRow)
		if amended == nil {
			t.Fatal("expected amended permission question")
		}
		got := plan(t, planRequest(amended.GetId(), permAmendedRow, option(0)))
		if !got.GetIsQuestionPending() || len(got.GetInputs()) != 0 || got.GetReason() == "" {
			t.Fatalf("plan = %v, want pending refusal for amend row", got)
		}
		// Row 2 ("No") is still selectable.
		no := plan(t, planRequest(amended.GetId(), permAmendedRow, option(1)))
		want := &bossanovav1.PlanQuestionAnswerResponse{
			IsQuestionPending: true,
			Inputs:            []*bossanovav1.PaneInput{key("2")},
		}
		if !proto.Equal(no, want) {
			t.Fatalf("plan(No) = %v, want %v", no, want)
		}
	})
}
