package session

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	grpcstatus "google.golang.org/grpc/status"
)

func (h *startTmuxChatHarness) paneInputCalls() bool {
	return h.tmuxFake.hasSubcommand("load-buffer") || h.tmuxFake.hasSubcommand("paste-buffer") || h.tmuxFake.hasSubcommand("send-keys")
}

// A prefill delivery is for someone else to submit, so it must never reach the
// runner as a startup input: even a runner that consumes startup input (and
// would auto-submit it from argv) gets nothing, and the pane prefill path runs.
func TestStartTmuxChat_PrefillPassesNoInitialInput(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping slow tmux test in -short; run make test-bossd for coverage")
	}
	h := newStartTmuxChatHarness(t)
	h.agentFake.ConsumesInitialInput = true
	h.agentFake.InitialInputFileMode = 0o600

	if _, err := h.lc.StartTmuxChat(context.Background(), "sess-1", ChatInput{Prompt: "draft for the user"}, "T", HookOpts{}); err != nil {
		t.Fatalf("StartTmuxChat: %v", err)
	}
	req := h.agentFake.LastBuildInteractiveCommand
	if req.GetInitialPrompt() != "" || req.GetInitialCommand() != "" || req.GetInitialInputPath() != "" {
		t.Fatalf("prefill leaked a startup input to the runner: prompt=%q command=%q path=%q",
			req.GetInitialPrompt(), req.GetInitialCommand(), req.GetInitialInputPath())
	}
	if joined := strings.Join(h.findCall("new-session").args, " "); strings.Contains(joined, "draft for the user") {
		t.Fatalf("prefill input must not be in the launch argv: %s", joined)
	}
	if !h.paneInputCalls() {
		t.Fatal("prefill input must still be delivered through the pane")
	}
}

// A submitted first input gets a fresh private file beside the agent log —
// never inside the worktree — and launches with argv that references it.
func TestStartTmuxChat_SubmitDeliversInputThroughPrivateFile(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping slow tmux test in -short; run make test-bossd for coverage")
	}
	h := newStartTmuxChatHarness(t)
	h.agentFake.ConsumesInitialInput = true
	h.agentFake.InitialInputFileMode = 0o600
	prompt := strings.Repeat("long prompt ", 3500) // ~40 KiB

	if _, err := h.lc.StartTmuxChat(context.Background(), "sess-1", ChatInput{Prompt: prompt, Delivery: DeliverySubmit}, "T", HookOpts{}); err != nil {
		t.Fatalf("StartTmuxChat: %v", err)
	}
	path := h.agentFake.LastBuildInteractiveCommand.GetInitialInputPath()
	if path == "" {
		t.Fatal("submitted input must come with an initial_input_path")
	}
	if filepath.Dir(path) != h.logsDir {
		t.Fatalf("input file %q must sit beside the agent log in %q", path, h.logsDir)
	}
	wt := h.sessions.sessions["sess-1"].WorktreePath
	if rel, err := filepath.Rel(wt, path); err == nil && !strings.HasPrefix(rel, "..") {
		t.Fatalf("input file %q must be outside the worktree %q", path, wt)
	}
	newSess := h.findCall("new-session")
	if newSess == nil {
		t.Fatal("expected tmux new-session")
	}
	if !slices.Contains(newSess.args, path) || strings.Contains(strings.Join(newSess.args, " "), "long prompt") {
		t.Fatalf("launch argv must reference the file, not carry the prompt: %d args", len(newSess.args))
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("input file must survive a successful launch for the shell to read: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("input file mode = %o, want 600", perm)
	}
	if h.paneInputCalls() {
		t.Fatal("consumed input must not also be typed into the pane")
	}
}

// Each launch gets its own file, so a relaunch can never read a predecessor's.
func TestStartTmuxChat_InputFilePathIsUniquePerLaunch(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping slow tmux test in -short; run make test-bossd for coverage")
	}
	a, err := newInitialInputPath(t.TempDir(), "agent-1")
	if err != nil {
		t.Fatal(err)
	}
	b, err := newInitialInputPath(filepath.Dir(a), "agent-1")
	if err != nil {
		t.Fatal(err)
	}
	if a == b {
		t.Fatalf("two launches of one chat got the same input path %q", a)
	}
}

func TestStartTmuxChat_RefusesUnsafeInputFileBeforeLaunch(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping slow tmux test in -short; run make test-bossd for coverage")
	}
	cases := map[string]func(h *startTmuxChatHarness){
		"group/other readable": func(h *startTmuxChatHarness) { h.agentFake.InitialInputFileMode = 0o644 },
		"never written":        func(h *startTmuxChatHarness) { h.agentFake.ReferenceInitialInputFileOnly = true },
	}
	for name, arrange := range cases {
		t.Run(name, func(t *testing.T) {
			h := newStartTmuxChatHarness(t)
			h.agentFake.ConsumesInitialInput = true
			arrange(h)

			_, err := h.lc.StartTmuxChat(context.Background(), "sess-1", ChatInput{Prompt: "go", Delivery: DeliverySubmit}, "T", HookOpts{})
			if grpcstatus.Code(err) != codes.FailedPrecondition {
				t.Fatalf("StartTmuxChat err = %v, want FailedPrecondition", err)
			}
			if h.findCall("new-session") != nil {
				t.Fatal("tmux must not launch with an unverified input file")
			}
			path := h.agentFake.LastBuildInteractiveCommand.GetInitialInputPath()
			if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("refused input file must be removed; stat err=%v", err)
			}
		})
	}
}

func TestStartTmuxChat_RemovesInputFileWhenLaunchFails(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping slow tmux test in -short; run make test-bossd for coverage")
	}
	h := newStartTmuxChatHarness(t)
	h.agentFake.ConsumesInitialInput = true
	h.agentFake.InitialInputFileMode = 0o600
	h.tmuxFake.failSubcommand["new-session"] = true

	if _, err := h.lc.StartTmuxChat(context.Background(), "sess-1", ChatInput{Prompt: "go now", Delivery: DeliverySubmit}, "T", HookOpts{}); err == nil {
		t.Fatal("StartTmuxChat succeeded with a failing tmux launch")
	}
	path := h.agentFake.LastBuildInteractiveCommand.GetInitialInputPath()
	if path == "" {
		t.Fatal("expected an initial_input_path")
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("input file must be removed when the start fails; stat err=%v", err)
	}
}

// A runner that does not consume the input leaves nothing behind and falls
// back to typing the input into the pane.
func TestStartTmuxChat_UnconsumedInputLeavesNoFile(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping slow tmux test in -short; run make test-bossd for coverage")
	}
	h := newStartTmuxChatHarness(t)

	if _, err := h.lc.StartTmuxChat(context.Background(), "sess-1", ChatInput{Prompt: "doctor", Delivery: DeliverySubmit}, "T", HookOpts{}); err != nil {
		t.Fatalf("StartTmuxChat: %v", err)
	}
	entries, err := os.ReadDir(h.logsDir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), initialInputFileSuffix) {
			t.Fatalf("unconsumed input left a file behind: %s", e.Name())
		}
	}
	if !h.paneInputCalls() {
		t.Fatal("unconsumed input must be delivered through the pane")
	}
}

// The startup sweep removes only day-old input files: a fresh one may belong to
// a launch still starting, and nothing else in the logs dir is touched.
func TestSweepStaleInitialInputFiles(t *testing.T) {
	dir := t.TempDir()
	lc := &Lifecycle{agentLogsDir: dir}
	now := time.Now()
	write := func(name string, age time.Duration) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(p, now.Add(-age), now.Add(-age)); err != nil {
			t.Fatal(err)
		}
		return p
	}
	stale := write("agent-1.0011223344556677"+initialInputFileSuffix, 25*time.Hour)
	fresh := write("agent-2.8899aabbccddeeff"+initialInputFileSuffix, time.Hour)
	log := write("agent-1.log", 48*time.Hour)

	if n := lc.SweepStaleInitialInputFiles(now); n != 1 {
		t.Fatalf("removed = %d, want 1", n)
	}
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Fatalf("stale input file survived: %v", err)
	}
	for _, keep := range []string{fresh, log} {
		if _, err := os.Stat(keep); err != nil {
			t.Fatalf("%s must be kept: %v", keep, err)
		}
	}
	if n := (&Lifecycle{}).SweepStaleInitialInputFiles(now); n != 0 {
		t.Fatalf("no logs dir must be a no-op, removed %d", n)
	}
}
