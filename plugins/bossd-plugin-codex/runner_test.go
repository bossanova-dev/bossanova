package main

import (
	"reflect"
	"testing"

	"github.com/rs/zerolog"

	"github.com/recurser/bossalib/agentruntime"
)

// TestBuildArgvBasicSession asserts the headless argv shape: codex generates
// its own UUID via thread.started, so neither --session-id nor any positional
// session ID belongs in the argv. ProvidedSessionID is ignored.
func TestBuildArgvBasicSession(t *testing.T) {
	r := NewRunner(zerolog.Nop())
	got := r.buildArgv(agentruntime.BuildArgvInput{
		SessionID: "sess-1", ProvidedSessionID: true,
	})
	want := []string{"codex", "exec", "--json", "--skip-git-repo-check"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("buildArgv = %v, want %v", got, want)
	}
}

// TestBuildArgvIncludesResume asserts that resume is the positional
// `codex exec resume <UUID>` subcommand form, not a flag, and appears
// before --json/--skip-git-repo-check.
func TestBuildArgvIncludesResume(t *testing.T) {
	r := NewRunner(zerolog.Nop())
	resume := "abcd-1234"
	got := r.buildArgv(agentruntime.BuildArgvInput{
		Resume: &resume, SessionID: "sess-1", ProvidedSessionID: true,
	})
	want := []string{"codex", "exec", "resume", "abcd-1234", "--json", "--skip-git-repo-check"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("buildArgv (resume) = %v, want %v", got, want)
	}
}

// TestBuildArgvIncludesSandboxWhenSet asserts WithSandbox emits the codex
// `--sandbox <mode>` flag. Modes: workspace-write, read-only, danger-full-access.
func TestBuildArgvIncludesSandboxWhenSet(t *testing.T) {
	r := NewRunner(zerolog.Nop(), WithSandbox("workspace-write"))
	got := r.buildArgv(agentruntime.BuildArgvInput{SessionID: "x", ProvidedSessionID: true})
	if !contains(got, "--sandbox") || !contains(got, "workspace-write") {
		t.Errorf("expected --sandbox workspace-write in %v", got)
	}
}

// TestBuildArgvIncludesApprovalWhenSet asserts WithApproval emits the codex
// `--ask-for-approval <policy>` flag (note: --ask-for-approval, not --approval).
func TestBuildArgvIncludesApprovalWhenSet(t *testing.T) {
	r := NewRunner(zerolog.Nop(), WithApproval("on-request"))
	got := r.buildArgv(agentruntime.BuildArgvInput{SessionID: "x", ProvidedSessionID: true})
	if !contains(got, "--ask-for-approval") || !contains(got, "on-request") {
		t.Errorf("expected --ask-for-approval on-request in %v", got)
	}
}

// TestBuildArgvIncludesDangerouslyBypassWhenSet asserts that the
// dangerously-bypass toggle emits codex's
// `--dangerously-bypass-approvals-and-sandbox` flag.
func TestBuildArgvIncludesDangerouslyBypassWhenSet(t *testing.T) {
	r := NewRunner(zerolog.Nop(), WithDangerouslyBypassApprovalsAndSandbox(true))
	got := r.buildArgv(agentruntime.BuildArgvInput{SessionID: "x", ProvidedSessionID: true})
	if !contains(got, "--dangerously-bypass-approvals-and-sandbox") {
		t.Errorf("expected --dangerously-bypass-approvals-and-sandbox in %v", got)
	}
}

// TestBuildArgvBypassOverridesSandboxAndApproval asserts that codex rejects
// --sandbox / --ask-for-approval combined with the dangerously-bypass flag,
// so the runner must drop sandbox/approval flags when bypass is on.
func TestBuildArgvBypassOverridesSandboxAndApproval(t *testing.T) {
	r := NewRunner(zerolog.Nop(),
		WithSandbox("workspace-write"),
		WithApproval("on-request"),
		WithDangerouslyBypassApprovalsAndSandbox(true),
	)
	got := r.buildArgv(agentruntime.BuildArgvInput{SessionID: "x", ProvidedSessionID: true})
	if !contains(got, "--dangerously-bypass-approvals-and-sandbox") {
		t.Errorf("expected --dangerously-bypass-approvals-and-sandbox in %v", got)
	}
	if contains(got, "--sandbox") {
		t.Errorf("--sandbox must be omitted when bypass is on; got %v", got)
	}
	if contains(got, "--ask-for-approval") {
		t.Errorf("--ask-for-approval must be omitted when bypass is on; got %v", got)
	}
}

// TestBuildArgvOmitsBypassWhenFalse asserts the bypass flag is opt-in:
// constructing the runner without the option (or with false) leaves argv
// untouched so existing sandbox/approval defaults still apply.
func TestBuildArgvOmitsBypassWhenFalse(t *testing.T) {
	r := NewRunner(zerolog.Nop(), WithDangerouslyBypassApprovalsAndSandbox(false))
	got := r.buildArgv(agentruntime.BuildArgvInput{SessionID: "x", ProvidedSessionID: true})
	if contains(got, "--dangerously-bypass-approvals-and-sandbox") {
		t.Errorf("did not expect --dangerously-bypass-approvals-and-sandbox in %v", got)
	}
}

func contains(haystack []string, needle string) bool {
	for _, s := range haystack {
		if s == needle {
			return true
		}
	}
	return false
}

// TestParseThreadStartedID covers the parser's edge cases against
// stdout shapes the runner observes in practice: a banner before any
// JSON, garbled lines mixed with valid JSON, multiple thread.started
// events (only the first wins), an empty thread_id (treated as
// "no event"), and a transcript with no thread.started at all.
func TestParseThreadStartedID(t *testing.T) {
	tests := []struct {
		name string
		in   []byte
		want string
	}{
		{
			name: "banner before json",
			in: []byte(`Loading codex…
preparing session
{"type":"thread.started","thread_id":"banner-uuid"}
`),
			want: "banner-uuid",
		},
		{
			name: "garbled bytes mixed with valid json",
			in:   []byte("\x00\x01garbage\nnot json {oops\n{\"type\":\"thread.started\",\"thread_id\":\"after-garbage\"}\n"),
			want: "after-garbage",
		},
		{
			name: "multiple thread.started events — first wins",
			in: []byte(`{"type":"thread.started","thread_id":"first-id"}
{"type":"thread.started","thread_id":"second-id"}
`),
			want: "first-id",
		},
		{
			name: "empty thread_id is ignored",
			in: []byte(`{"type":"thread.started","thread_id":""}
{"type":"thread.started","thread_id":"real-id"}
`),
			want: "real-id",
		},
		{
			name: "no thread.started in stream",
			in: []byte(`{"type":"event_msg","payload":{"type":"task_started"}}
{"type":"turn.completed","payload":{"duration_ms":42}}
`),
			want: "",
		},
		{
			name: "empty input",
			in:   []byte(""),
			want: "",
		},
		{
			name: "wrong type field",
			in: []byte(`{"type":"thread.shutdown","thread_id":"abc"}
{"type":"thread_started","thread_id":"underscore-not-dot"}
`),
			want: "",
		},
		{
			name: "trailing partial line is ignored gracefully",
			in: []byte(`{"type":"thread.started","thread_id":"clean-id"}
{"type":"event_msg","payload":{"type":"agent_mes`),
			want: "clean-id",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := parseThreadStartedID(tt.in); got != tt.want {
				t.Errorf("parseThreadStartedID = %q, want %q", got, tt.want)
			}
		})
	}
}
