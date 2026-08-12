package main

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/recurser/boss/internal/views"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/spf13/pflag"
)

// TestNewDetachRequestSetsModel pins the BOS-179 `boss new --model` wiring: a
// non-empty model flows into CreateSessionRequest.model, an empty one leaves it
// unset (agent default), and the scripting path always requests a headless run.
func strptr(s string) *string { return &s }

func TestNewDetachRequestSetsModel(t *testing.T) {
	req := newDetachRequest(newSessionOpts{
		RepoID: "repo-1", Prompt: "do work", Title: "T",
		AgentName: "claude", Model: "claude-opus-4-8", Account: strptr("acct-1"),
	})
	if !req.GetDetach() {
		t.Fatalf("detach = false, want true for the scripting path")
	}
	if req.GetModel() != "claude-opus-4-8" {
		t.Fatalf("model = %q, want claude-opus-4-8", req.GetModel())
	}
	if req.GetAgentName() != "claude" {
		t.Fatalf("agent = %q, want claude", req.GetAgentName())
	}
	if req.GetAccountId() != "acct-1" {
		t.Fatalf("account = %q, want acct-1", req.GetAccountId())
	}

	empty := newDetachRequest(newSessionOpts{RepoID: "repo-1", Prompt: "do work", Title: "T"})
	if empty.Model != nil {
		t.Fatalf("model = %v, want nil (unset) when the flag is empty", empty.Model)
	}
	if empty.AgentName != nil {
		t.Fatalf("agent = %v, want nil (unset) when the flag is empty", empty.AgentName)
	}
	if empty.AccountId != nil {
		t.Fatalf("account = %v, want nil (unset) when the flag is absent", empty.AccountId)
	}
}

// TestNewDetachRequestSetsAccount pins the BOS-170 `boss new --account` wiring:
// a non-empty account flows into CreateSessionRequest.account_id.
func TestNewDetachRequestSetsAccount(t *testing.T) {
	req := newDetachRequest(newSessionOpts{
		RepoID: "repo-1", Prompt: "do work", Title: "T", Account: strptr("team-openai"),
	})
	if req.GetAccountId() != "team-openai" {
		t.Fatalf("account = %q, want team-openai", req.GetAccountId())
	}
}

// TestNewDetachRequestAccountPresence pins the account-0 opt-out distinction: a
// present-empty account pointer (explicit `--account=`) forwards a present-empty
// account_id (skips the default-account policy), while a nil pointer (omitted
// flag) leaves account_id unset (the daemon applies its policy).
func TestNewDetachRequestAccountPresence(t *testing.T) {
	explicit := newDetachRequest(newSessionOpts{
		RepoID: "repo-1", Prompt: "do work", Title: "T", Account: strptr(""),
	})
	if explicit.AccountId == nil {
		t.Fatal("explicit --account=: account_id nil, want present-empty")
	}
	if *explicit.AccountId != "" {
		t.Fatalf("explicit --account=: account_id = %q, want present-empty \"\"", *explicit.AccountId)
	}

	absent := newDetachRequest(newSessionOpts{RepoID: "repo-1", Prompt: "do work", Title: "T"})
	if absent.AccountId != nil {
		t.Fatalf("omitted flag: account_id = %v, want nil (unset)", absent.AccountId)
	}
}

// TestNewCmdRegistersAccountFlag guards the flag surface so `boss new
// --account` stays wired.
func TestNewCmdRegistersAccountFlag(t *testing.T) {
	cmd := newCmd()
	f := cmd.Flags().Lookup("account")
	if f == nil {
		t.Fatal("`boss new` is missing the --account flag")
	}
	if f.DefValue != "" {
		t.Fatalf("--account default = %q, want empty", f.DefValue)
	}
}

// TestAccountShowLabel pins the `boss show` Account line: an unbound session
// renders "Unmanaged local credentials", a bound one with no server label renders its
// account id verbatim, and a server-computed label wins when present.
func TestAccountShowLabel(t *testing.T) {
	if got := accountShowLabel("", ""); got != views.UnmanagedLocalCredentialsLabel {
		t.Fatalf("accountShowLabel(\"\", \"\") = %q, want %q", got, views.UnmanagedLocalCredentialsLabel)
	}
	if got := accountShowLabel("acct-9", ""); got != "acct-9" {
		t.Fatalf("accountShowLabel(acct-9, \"\") = %q, want acct-9", got)
	}
	if got := accountShowLabel("acct-9", "Work Claude"); got != "Work Claude" {
		t.Fatalf("accountShowLabel(acct-9, Work Claude) = %q, want Work Claude", got)
	}
}

// TestPrintSessionShowHeaderRendersAccountLine asserts `boss show` prints an
// Account line for both bound and unbound sessions.
func TestPrintSessionShowHeaderRendersAccountLine(t *testing.T) {
	bound := "team-openai"
	out := captureStdout(t, func() {
		printSessionShowHeader(&pb.Session{Id: "sess-1234abcd", AccountId: &bound})
	})
	if !strings.Contains(out, "Account:  team-openai") {
		t.Fatalf("show header missing bound Account line; got:\n%s", out)
	}

	unbound := captureStdout(t, func() {
		printSessionShowHeader(&pb.Session{Id: "sess-1234abcd"})
	})
	if !strings.Contains(unbound, "Account:  "+views.UnmanagedLocalCredentialsLabel) {
		t.Fatalf("show header missing unbound Account line; got:\n%s", unbound)
	}
}

// TestNewCmdRegistersModelFlag guards the flag surface so `boss new --model`
// stays wired.
func TestNewCmdRegistersModelFlag(t *testing.T) {
	cmd := newCmd()
	f := cmd.Flags().Lookup("model")
	if f == nil {
		t.Fatal("`boss new` is missing the --model flag")
	}
	if f.DefValue != "" {
		t.Fatalf("--model default = %q, want empty", f.DefValue)
	}
}

// TestNewDetachRequestTmuxUnattended pins the BOS-821 `--tmux-unattended`
// wiring. The two fields are ORTHOGONAL on the daemon — StartSessionOpts
// carries Detach and IsTmuxUnattended independently and mints a hook token when
// EITHER is set (services/bossd/internal/server/server.go) — so the tempting
// simplification of treating them as alternatives is wrong. Pin both: the flag
// must set IsTmuxUnattended without clearing the scripting path's Detach.
func TestNewDetachRequestTmuxUnattended(t *testing.T) {
	req := newDetachRequest(newSessionOpts{
		RepoID: "repo-1", Prompt: "do work", Title: "T", TmuxUnattended: true,
	})
	if !req.GetIsTmuxUnattended() {
		t.Fatalf("is_tmux_unattended = false, want true when --tmux-unattended is set")
	}
	if !req.GetDetach() {
		t.Fatalf("detach = false, want true — --tmux-unattended must not clear Detach")
	}

	off := newDetachRequest(newSessionOpts{RepoID: "repo-1", Prompt: "do work", Title: "T"})
	if off.GetIsTmuxUnattended() {
		t.Fatalf("is_tmux_unattended = true, want false when the flag is omitted")
	}
	if !off.GetDetach() {
		t.Fatalf("detach = false, want true for the scripting path")
	}
}

// TestNewDetachRequestDeferPR pins the `--defer-pr` wiring: the flag reaches
// defer_pr and must not synthesize is_quick_chat, which selects an entirely
// different daemon path (StartQuickChatSession, no worktree at all).
func TestNewDetachRequestDeferPR(t *testing.T) {
	req := newDetachRequest(newSessionOpts{
		RepoID: "repo-1", Prompt: "do work", Title: "T", DeferPR: true,
	})
	if !req.GetDeferPr() {
		t.Fatalf("defer_pr = false, want true when --defer-pr is set")
	}
	if req.GetIsQuickChat() {
		t.Fatalf("is_quick_chat = true; --defer-pr must not set it")
	}

	off := newDetachRequest(newSessionOpts{RepoID: "repo-1", Prompt: "do work", Title: "T"})
	if off.GetDeferPr() {
		t.Fatalf("defer_pr = true, want false when the flag is omitted")
	}
}

// TestNewDetachRequestQuickChat pins the `--quick-chat` wiring, mirrored: the
// flag reaches is_quick_chat and leaves defer_pr alone.
func TestNewDetachRequestQuickChat(t *testing.T) {
	req := newDetachRequest(newSessionOpts{
		RepoID: "repo-1", Prompt: "do work", Title: "T", QuickChat: true,
	})
	if !req.GetIsQuickChat() {
		t.Fatalf("is_quick_chat = false, want true when --quick-chat is set")
	}
	if req.GetDeferPr() {
		t.Fatalf("defer_pr = true; --quick-chat must not set it")
	}

	off := newDetachRequest(newSessionOpts{RepoID: "repo-1", Prompt: "do work", Title: "T"})
	if off.GetIsQuickChat() {
		t.Fatalf("is_quick_chat = true, want false when the flag is omitted")
	}
}

// TestNewDetachRequestDeferPRAndQuickChatLeaveDetachSet is the regression guard
// for the specific way this feature can be broken silently. Detach is what makes
// the daemon mint the finalize hook token (services/bossd/internal/server/server.go),
// and that hook is exactly what opens --defer-pr's PR at finalize when the run
// DID produce commits. A plausible-looking "quick chat / deferred PR means we
// are not really detaching" simplification would clear Detach and disable the
// feature while every other assertion here still passed — so assert it head-on
// rather than relying on an incidental field check elsewhere.
func TestNewDetachRequestDeferPRAndQuickChatLeaveDetachSet(t *testing.T) {
	cases := []struct {
		name string
		opts newSessionOpts
	}{
		{"defer-pr", newSessionOpts{RepoID: "repo-1", Prompt: "do work", Title: "T", DeferPR: true}},
		{"quick-chat", newSessionOpts{RepoID: "repo-1", Prompt: "do work", Title: "T", QuickChat: true}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if req := newDetachRequest(tc.opts); !req.GetDetach() {
				t.Fatalf("detach = false, want true — --%s must not clear Detach, "+
					"which mints the finalize hook token", tc.name)
			}
		})
	}
}

// TestNewCmdRegistersDeferPRAndQuickChatFlags guards the flag surface so both
// stay wired and documented in `boss new --help`.
func TestNewCmdRegistersDeferPRAndQuickChatFlags(t *testing.T) {
	cmd := newCmd()
	for _, name := range []string{"defer-pr", "quick-chat"} {
		f := cmd.Flags().Lookup(name)
		if f == nil {
			t.Fatalf("`boss new` is missing the --%s flag", name)
		}
		if f.Value.Type() != "bool" {
			t.Errorf("--%s type = %q, want bool", name, f.Value.Type())
		}
		if f.DefValue != "false" {
			t.Errorf("--%s default = %q, want false", name, f.DefValue)
		}
		if f.Usage == "" {
			t.Errorf("--%s has no usage text, so it documents nothing in --help", name)
		}
	}
}

// TestNewFlagHelpIsProjectAgnostic pins the constraint that makes these usage
// strings safe to publish. `boss new`'s help is harvested into the globally
// installed `boss` skill core, which TestPublishedCoresAreProjectAgnostic
// forbids from naming this project's backlog. Catching it here names the actual
// cause at the flag, rather than as a mystified failure in the manifest test.
func TestNewFlagHelpIsProjectAgnostic(t *testing.T) {
	cmd := newCmd()
	// Assembled at runtime so this test file does not itself contain the literal
	// token the published-core guard scans for.
	forbidden := []string{"BOS" + "-", "bossanova-", "Internal Bossanova"}
	cmd.Flags().VisitAll(func(f *pflag.Flag) {
		for _, bad := range forbidden {
			if strings.Contains(f.Usage, bad) {
				t.Errorf("--%s usage contains project-specific token %q: %q", f.Name, bad, f.Usage)
			}
		}
	})
}

// TestNewSessionJSONNextAction pins the --json envelope's idle signal. A session
// that came back with no chat id launched no agent, so the envelope must say so;
// an ordinary create must leave the key ABSENT, not present-and-empty, because
// `omitempty` is what keeps the envelope byte-identical for existing callers.
// Asserted on the marshalled BYTES — unmarshalling into a struct cannot tell an
// absent key from a zero-value one, so it would pass against the regression.
func TestNewSessionJSONNextAction(t *testing.T) {
	idle, err := json.Marshal(newSessionJSON(&pb.Session{Id: "sess-1", Title: "T"}))
	if err != nil {
		t.Fatalf("marshal idle envelope: %v", err)
	}
	if !strings.Contains(string(idle), `"next_action"`) {
		t.Errorf("idle envelope is missing next_action: %s", idle)
	}

	launched, err := json.Marshal(newSessionJSON(&pb.Session{
		Id: "sess-1", Title: "T", AgentSessionId: strptr("chat-1"),
	}))
	if err != nil {
		t.Fatalf("marshal launched envelope: %v", err)
	}
	if strings.Contains(string(launched), "next_action") {
		t.Errorf("ordinary create's envelope carries next_action; omitempty regressed: %s", launched)
	}
}

// TestNewDetachRequestTrackerFields pins the BOS-821 tracker wiring: each flag
// reaches its own optional proto field as a SET pointer, and an omitted flag
// leaves the field nil rather than a pointer to "". The distinction matters
// because these are `optional` in the proto — present-but-empty is a different
// wire signal from absent, and the daemon's tracker-id dedup keys on presence.
func TestNewDetachRequestTrackerFields(t *testing.T) {
	req := newDetachRequest(newSessionOpts{
		RepoID: "repo-1", Prompt: "do work", Title: "T",
		TrackerID:     "BOS-821",
		TrackerSource: "linear",
		TrackerURL:    "https://linear.app/x/issue/BOS-821",
	})
	if req.GetTrackerId() != "BOS-821" {
		t.Fatalf("tracker_id = %q, want BOS-821", req.GetTrackerId())
	}
	if req.GetTrackerSource() != "linear" {
		t.Fatalf("tracker_source = %q, want linear", req.GetTrackerSource())
	}
	if req.GetTrackerUrl() != "https://linear.app/x/issue/BOS-821" {
		t.Fatalf("tracker_url = %q, want the supplied URL", req.GetTrackerUrl())
	}

	// Each field is independent: setting one must not synthesize the others.
	only := newDetachRequest(newSessionOpts{
		RepoID: "repo-1", Prompt: "do work", Title: "T", TrackerID: "BOS-821",
	})
	if only.TrackerId == nil || *only.TrackerId != "BOS-821" {
		t.Fatalf("tracker_id = %v, want a pointer to BOS-821", only.TrackerId)
	}
	if only.TrackerSource != nil {
		t.Fatalf("tracker_source = %q, want nil (unset) when the flag is absent", *only.TrackerSource)
	}
	if only.TrackerUrl != nil {
		t.Fatalf("tracker_url = %q, want nil (unset) when the flag is absent", *only.TrackerUrl)
	}

	absent := newDetachRequest(newSessionOpts{RepoID: "repo-1", Prompt: "do work", Title: "T"})
	if absent.TrackerId != nil {
		t.Fatalf("tracker_id = %q, want nil (unset) when the flag is absent", *absent.TrackerId)
	}
	if absent.TrackerSource != nil {
		t.Fatalf("tracker_source = %q, want nil (unset) when the flag is absent", *absent.TrackerSource)
	}
	if absent.TrackerUrl != nil {
		t.Fatalf("tracker_url = %q, want nil (unset) when the flag is absent", *absent.TrackerUrl)
	}
}

// TestNewDetachRequestPromptIsByteIdentical is the auto-submit regression
// guard. The daemon only auto-submits an unattended prompt when it is one
// trimmed line starting with `/` or `$`, so any wrapping, prefixing, trimming
// or reflowing the CLI applied on the way in would silently break the contract.
// Plan must be the flag value verbatim, whitespace and newlines included.
func TestNewDetachRequestPromptIsByteIdentical(t *testing.T) {
	prompts := []string{
		"/boss-build BOS-821",
		"  /boss-build BOS-821  ",
		"\n/boss-build BOS-821\n",
		"$ echo hi",
		"line one\nline two\n\nline four",
		"trailing spaces   ",
		"\t/tabbed",
	}
	for _, prompt := range prompts {
		req := newDetachRequest(newSessionOpts{RepoID: "repo-1", Prompt: prompt, Title: "T"})
		if req.GetPlan() != prompt {
			t.Errorf("plan = %q, want the prompt byte-identical (%q)", req.GetPlan(), prompt)
		}
	}
}

// TestNewCmdRegistersBOS821Flags guards the flag surface so the durable-pane,
// tracker and JSON flags stay wired and visible in `boss new --help`.
func TestNewCmdRegistersBOS821Flags(t *testing.T) {
	cmd := newCmd()
	for _, name := range []string{"tracker-id", "tracker-source", "tracker-url"} {
		f := cmd.Flags().Lookup(name)
		if f == nil {
			t.Fatalf("`boss new` is missing the --%s flag", name)
		}
		if f.DefValue != "" {
			t.Errorf("--%s default = %q, want empty", name, f.DefValue)
		}
		if f.Usage == "" {
			t.Errorf("--%s has no usage text, so it documents nothing in --help", name)
		}
	}
	for _, name := range []string{"tmux-unattended", "json"} {
		f := cmd.Flags().Lookup(name)
		if f == nil {
			t.Fatalf("`boss new` is missing the --%s flag", name)
		}
		if f.DefValue != "false" {
			t.Errorf("--%s default = %q, want false", name, f.DefValue)
		}
		if f.Usage == "" {
			t.Errorf("--%s has no usage text, so it documents nothing in --help", name)
		}
	}
}

// TestNewCmdDetachHelpDistinguishesTmuxUnattended pins the corrected --detach
// help text. `runNewDetach` ignores the flag — the non-interactive --repo +
// --prompt path always detaches — so help that implies a choice is a lie, and
// the durable-pane option a caller actually wants is --tmux-unattended.
func TestNewCmdDetachHelpDistinguishesTmuxUnattended(t *testing.T) {
	usage := newCmd().Flags().Lookup("detach").Usage
	for _, want := range []string{"--repo", "--prompt", "always detaches", "no-op", "--tmux-unattended"} {
		if !strings.Contains(usage, want) {
			t.Errorf("--detach usage %q is missing %q", usage, want)
		}
	}
}

// TestValidateTrackerSource pins the accepted vocabulary. The proto documents
// tracker_source as "linear" | "sentry" (proto/bossanova/v1/daemon.proto), and
// the daemon does not validate it, so the CLI is the only place a typo can be
// caught before it becomes a silently-mislabelled session.
func TestValidateTrackerSource(t *testing.T) {
	for _, ok := range []string{"", "linear", "sentry"} {
		if err := validateTrackerSource(ok); err != nil {
			t.Errorf("validateTrackerSource(%q) = %v, want nil", ok, err)
		}
	}
	for _, bad := range []string{"Linear", "jira", "github", " linear", "linear "} {
		err := validateTrackerSource(bad)
		if err == nil {
			t.Errorf("validateTrackerSource(%q) = nil, want an error", bad)
			continue
		}
		if got := errorCodeFor(err); got != codeInvalidArgument {
			t.Errorf("validateTrackerSource(%q) code = %q, want %q", bad, got, codeInvalidArgument)
		}
	}
}
