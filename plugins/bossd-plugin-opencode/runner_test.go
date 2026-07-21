package main

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/rs/zerolog"

	"github.com/recurser/bossalib/agentruntime"
)

func ptr(s string) *string { return &s }

// TestBuildArgv is the table-driven golden-argv suite. It asserts the exact
// headless argv shape across every documented configuration: fresh vs resume,
// model set (env and per-request) vs unset, the default --auto vs the
// --dangerously-skip-permissions escape hatch, the caller-supplied session-id
// hint being ignored, and the login-shell wrap.
func TestBuildArgv(t *testing.T) {
	const wd = "/work"
	base := []string{"opencode", "run", "--format", "json", "--dir", wd}

	tests := []struct {
		name string
		opts []Option
		in   agentruntime.BuildArgvInput
		want []string
	}{
		{
			name: "fresh run, default permission flag, no model",
			in:   agentruntime.BuildArgvInput{WorkDir: wd},
			want: append(append([]string{}, base...), "--auto"),
		},
		{
			name: "pre-1.18 CLI (v1.17.11) falls back to skip-permissions",
			opts: []Option{WithCLIVersion("1.17.11")},
			in:   agentruntime.BuildArgvInput{WorkDir: wd},
			want: append(append([]string{}, base...), "--dangerously-skip-permissions"),
		},
		{
			name: "unknown CLI version falls back to skip-permissions",
			opts: []Option{WithCLIVersion("")},
			in:   agentruntime.BuildArgvInput{WorkDir: wd},
			want: append(append([]string{}, base...), "--dangerously-skip-permissions"),
		},
		{
			name: "dangerously-skip-permissions escape hatch",
			opts: []Option{WithDangerouslySkipPermissions(true)},
			in:   agentruntime.BuildArgvInput{WorkDir: wd},
			want: append(append([]string{}, base...), "--dangerously-skip-permissions"),
		},
		{
			name: "model from env default",
			opts: []Option{WithModel("anthropic/claude-sonnet")},
			in:   agentruntime.BuildArgvInput{WorkDir: wd},
			want: append(append([]string{}, base...), "--auto", "--model", "anthropic/claude-sonnet"),
		},
		{
			name: "per-request model overrides env default",
			opts: []Option{WithModel("anthropic/claude-sonnet")},
			in:   agentruntime.BuildArgvInput{WorkDir: wd, Options: map[string]string{"model": "openai/gpt-5"}},
			want: append(append([]string{}, base...), "--auto", "--model", "openai/gpt-5"),
		},
		{
			name: "resume appends --session from in.Resume",
			in:   agentruntime.BuildArgvInput{WorkDir: wd, Resume: ptr("ses_abc123")},
			want: append(append([]string{}, base...), "--auto", "--session", "ses_abc123"),
		},
		{
			name: "resume with model — fixed ordering flags,model,session",
			opts: []Option{WithModel("anthropic/claude-sonnet")},
			in:   agentruntime.BuildArgvInput{WorkDir: wd, Resume: ptr("ses_abc123")},
			want: append(append([]string{}, base...), "--auto", "--model", "anthropic/claude-sonnet", "--session", "ses_abc123"),
		},
		{
			name: "caller session-id hint is ignored on a fresh run",
			in:   agentruntime.BuildArgvInput{WorkDir: wd, SessionID: "ses_hint", ProvidedSessionID: true},
			want: append(append([]string{}, base...), "--auto"),
		},
		{
			name: "login-shell wrap when configured",
			opts: []Option{WithLoginShell("/opt/homebrew/bin/fish")},
			in:   agentruntime.BuildArgvInput{WorkDir: wd},
			want: []string{
				"/opt/homebrew/bin/fish", "-l", "-i", "-c", "exec $argv",
				"opencode", "run", "--format", "json", "--dir", wd, "--auto",
			},
		},
		{
			name: "fresh run appends the plan as the positional message, last",
			in:   agentruntime.BuildArgvInput{WorkDir: wd, Plan: "do the thing"},
			want: append(append([]string{}, base...), "--auto", "do the thing"),
		},
		{
			name: "resume with model — plan is last, after flags/model/session",
			opts: []Option{WithModel("anthropic/claude-sonnet")},
			in:   agentruntime.BuildArgvInput{WorkDir: wd, Resume: ptr("ses_abc123"), Plan: "keep going"},
			want: append(append([]string{}, base...), "--auto", "--model", "anthropic/claude-sonnet", "--session", "ses_abc123", "keep going"),
		},
		{
			name: "multi-line plan rides argv as ONE element through the login-shell wrap",
			opts: []Option{WithLoginShell("/opt/homebrew/bin/fish")},
			in:   agentruntime.BuildArgvInput{WorkDir: wd, Plan: "line one\nline two"},
			want: []string{
				"/opt/homebrew/bin/fish", "-l", "-i", "-c", "exec $argv",
				"opencode", "run", "--format", "json", "--dir", wd, "--auto", "line one\nline two",
			},
		},
		{
			name: "empty plan appends no positional message",
			in:   agentruntime.BuildArgvInput{WorkDir: wd, Plan: ""},
			want: append(append([]string{}, base...), "--auto"),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Default the CLI version to a release that supports --auto so the
			// permission-flag cases below assert the documented default; a case
			// may override by appending its own WithCLIVersion (last wins).
			opts := append([]Option{WithCLIVersion("1.18.3")}, tt.opts...)
			r := NewRunner(zerolog.Nop(), opts...)
			got := r.buildArgv(tt.in)
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("buildArgv =\n  %#v\nwant\n  %#v", got, tt.want)
			}
		})
	}
}

// TestBuildArgvAlwaysExactlyOnePermissionFlag is the hard invariant: no matter
// the configuration, exactly one of --auto / --dangerously-skip-permissions is
// present. A tool-using run with no permission flag would hang forever on an
// unanswerable prompt in an unattended worktree.
func TestBuildArgvAlwaysExactlyOnePermissionFlag(t *testing.T) {
	configs := [][]Option{
		nil,
		{WithDangerouslySkipPermissions(true)},
		{WithDangerouslySkipPermissions(false)},
		{WithModel("anthropic/claude-sonnet")},
		{WithLoginShell("/opt/homebrew/bin/fish")},
		{WithCLIVersion("1.18.3")},  // --auto branch
		{WithCLIVersion("1.17.11")}, // fallback branch
		{WithCLIVersion("2.0.0")},   // future major, --auto branch
		{WithCLIVersion("garbage")}, // unparseable, fallback branch
	}
	for i, opts := range configs {
		r := NewRunner(zerolog.Nop(), opts...)
		argv := r.buildArgv(agentruntime.BuildArgvInput{WorkDir: "/work", Resume: ptr("ses_x")})
		n := 0
		for _, a := range argv {
			if a == "--auto" || a == "--dangerously-skip-permissions" {
				n++
			}
		}
		if n != 1 {
			t.Errorf("config %d: got %d permission flags, want exactly 1 in %v", i, n, argv)
		}
	}
}

// TestAutoPermissionSupported pins the version floor for the --auto permission
// flag: it exists on opencode >= 1.18 and must fall back to
// --dangerously-skip-permissions on older or unparseable versions (BOS-437 live
// validation surfaced v1.17.11 rejecting --auto).
func TestAutoPermissionSupported(t *testing.T) {
	tests := []struct {
		version string
		want    bool
	}{
		{"1.18.3", true},
		{"1.18.0", true},
		{"v1.18.3", true},
		{"1.19.0", true},
		{"2.0.0", true},
		{"1.17.11", false},
		{"1.17.99", false},
		{"1.0.0", false},
		{"0.99.0", false},
		{"", false},
		{"garbage", false},
		{"1", false},
		{"1.x", false},
	}
	for _, tt := range tests {
		if got := autoPermissionSupported(tt.version); got != tt.want {
			t.Errorf("autoPermissionSupported(%q) = %v, want %v", tt.version, got, tt.want)
		}
	}
}

// TestNewRunnerIgnoresAmbientLoginShellEnv proves the login-shell wrap is only
// active via the explicit WithLoginShell option, never an ambient env var.
func TestNewRunnerIgnoresAmbientLoginShellEnv(t *testing.T) {
	t.Setenv("BOSS_PLUGIN_login_shell", "/bin/zsh")
	r := NewRunner(zerolog.Nop())
	if r.loginShell != "" {
		t.Fatalf("loginShell = %q, want empty without WithLoginShell option", r.loginShell)
	}
}

// TestSessionIDFromOutput covers the parser against the vendored real fixture
// plus the synthetic edge cases the runner observes in practice: a banner before
// any JSON, garbled bytes mixed with valid JSON, multiple events (only the first
// non-empty wins), an empty sessionID skipped, no sessionID at all, unknown
// event types tolerated, and a trailing partial line ignored gracefully.
func TestSessionIDFromOutput(t *testing.T) {
	freshFixture := readFixture(t, "run_fresh.jsonl")

	tests := []struct {
		name string
		in   []byte
		want string
	}{
		{
			name: "vendored fresh-run fixture — first event's sessionID",
			in:   freshFixture,
			want: "ses_7f3a2b1c9d8e4f5a6b7c",
		},
		{
			name: "vendored resume fixture — echoes its own id",
			in:   readFixture(t, "run_resume.jsonl"),
			want: "ses_1a2b3c4d5e6f7a8b9c0d",
		},
		{
			name: "banner before json",
			in: []byte(`opencode v1.18.3
starting session
{"type":"step_start","sessionID":"ses_bannerafterprose01"}
`),
			want: "ses_bannerafterprose01",
		},
		{
			name: "garbled bytes mixed with valid json",
			in:   []byte("\x00\x01garbage\nnot json {oops\n{\"type\":\"step_start\",\"sessionID\":\"ses_aftergarbage00001\"}\n"),
			want: "ses_aftergarbage00001",
		},
		{
			name: "multiple events — first non-empty wins",
			in: []byte(`{"type":"step_start","sessionID":"ses_firstevent0000001"}
{"type":"text","sessionID":"ses_secondevent000002"}
`),
			want: "ses_firstevent0000001",
		},
		{
			name: "empty sessionID is skipped",
			in: []byte(`{"type":"step_start","sessionID":""}
{"type":"text","sessionID":"ses_realafterempty0001"}
`),
			want: "ses_realafterempty0001",
		},
		{
			name: "no sessionID in stream",
			in: []byte(`{"type":"info","message":"warming up"}
{"type":"log","level":"debug"}
`),
			want: "",
		},
		{
			name: "empty input",
			in:   []byte(""),
			want: "",
		},
		{
			name: "unknown event type still yields its sessionID",
			in:   []byte(`{"type":"some_future_event","sessionID":"ses_unknownbutparsed01"}` + "\n"),
			want: "ses_unknownbutparsed01",
		},
		{
			name: "trailing partial line is ignored gracefully",
			in: []byte(`{"type":"step_start","sessionID":"ses_cleanbeforepartial"}
{"type":"text","sessionID":"ses_tru`),
			want: "ses_cleanbeforepartial",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := sessionIDFromOutput(tt.in); got != tt.want {
				t.Errorf("sessionIDFromOutput = %q, want %q", got, tt.want)
			}
		})
	}
}

// TestLiveOpencodeRunSkipsWhenAbsent is the sole binary-dependent step. It skips
// cleanly when the opencode binary is not on PATH (always the case in CI), so the
// hermetic fixture-driven suite above is the real coverage. When a real binary is
// present locally it smoke-checks that the version subcommand runs.
func TestLiveOpencodeRunSkipsWhenAbsent(t *testing.T) {
	bin, err := exec.LookPath("opencode")
	if err != nil {
		t.Skip("opencode binary not on PATH; skipping live-binary smoke (fixtures cover the run path)")
	}
	if out, err := exec.Command(bin, "--version").CombinedOutput(); err != nil {
		t.Skipf("opencode --version failed (%v); skipping live smoke: %s", err, out)
	}
}

func readFixture(t *testing.T, name string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("read fixture %s: %v", name, err)
	}
	return data
}

// wrapLog re-encodes raw opencode JSONL into the agentruntime lineWriter tail
// shape the PostExit hook actually receives: one NDJSON object per line,
// {"ts":"...","text":"<raw line>"}, with the raw event escaped inside `text`.
// It mirrors lineWriter.writeNDJSON so auth/cap tests exercise the classifiers
// against production-shaped input, not just raw fixtures (blank lines are
// dropped, matching the line-split writer).
func wrapLog(t *testing.T, raw []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	for _, line := range bytes.Split(raw, []byte{'\n'}) {
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		entry := struct {
			TS   string `json:"ts"`
			Text string `json:"text"`
		}{TS: "2026-07-08T10:00:00Z", Text: string(line)}
		data, err := json.Marshal(entry)
		if err != nil {
			t.Fatalf("wrapLog marshal: %v", err)
		}
		buf.Write(data)
		buf.WriteByte('\n')
	}
	return buf.Bytes()
}
