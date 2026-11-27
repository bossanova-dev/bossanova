package main

import (
	"context"
	"os/exec"
	"path/filepath"
	"slices"
	"sync"
	"testing"

	"github.com/rs/zerolog"

	bossanovav1 "github.com/recurser/bossalib/gen/bossanova/v1"
)

func TestUnattendedPolicyArgv(t *testing.T) {
	t.Run("headless", func(t *testing.T) {
		tests := []struct {
			name      string
			opts      []Option
			policy    *bossanovav1.UnattendedPermissionPolicy
			wantPairs [][2]string
			wantFlags []string
			forbid    []string
		}{
			{
				name: "policy_present_network_allowed",
				policy: &bossanovav1.UnattendedPermissionPolicy{
					WorktreePath:     "/tmp/wt",
					IsNetworkAllowed: true,
				},
				wantPairs: [][2]string{
					{"--sandbox", "workspace-write"},
					{"-c", `approval_policy="never"`},
					{"-c", "sandbox_workspace_write.network_access=true"},
				},
				forbid: []string{"--ask-for-approval", "--dangerously-bypass-approvals-and-sandbox"},
			},
			{
				name: "policy_present_network_disallowed",
				policy: &bossanovav1.UnattendedPermissionPolicy{
					WorktreePath:     "/tmp/wt",
					IsNetworkAllowed: false,
				},
				wantPairs: [][2]string{
					{"--sandbox", "workspace-write"},
					{"-c", `approval_policy="never"`},
				},
				forbid: []string{"--ask-for-approval", "sandbox_workspace_write.network_access=true", "--dangerously-bypass-approvals-and-sandbox"},
			},
			{
				name:   "policy_absent_unchanged",
				policy: nil,
				forbid: []string{"--sandbox", "--ask-for-approval", `approval_policy="never"`, "sandbox_workspace_write.network_access=true", "--dangerously-bypass-approvals-and-sandbox"},
			},
			{
				name: "policy_present_bypass_on_omits_policy",
				opts: []Option{WithDangerouslyBypassApprovalsAndSandbox(true)},
				policy: &bossanovav1.UnattendedPermissionPolicy{
					WorktreePath:     "/tmp/wt",
					IsNetworkAllowed: true,
				},
				wantFlags: []string{"--dangerously-bypass-approvals-and-sandbox"},
				forbid:    []string{"--sandbox", "--ask-for-approval", `approval_policy="never"`, "sandbox_workspace_write.network_access=true"},
			},
			{
				name: "explicit_sandbox_overrides_policy_sandbox",
				opts: []Option{WithSandbox("read-only")},
				policy: &bossanovav1.UnattendedPermissionPolicy{
					WorktreePath:     "/tmp/wt",
					IsNetworkAllowed: true,
				},
				wantPairs: [][2]string{
					{"--sandbox", "read-only"},
					{"-c", `approval_policy="never"`},
					{"-c", "sandbox_workspace_write.network_access=true"},
				},
				forbid: []string{"workspace-write", "--ask-for-approval"},
			},
			{
				name: "explicit_approval_overrides_policy_approval",
				opts: []Option{WithApproval("on-request")},
				policy: &bossanovav1.UnattendedPermissionPolicy{
					WorktreePath:     "/tmp/wt",
					IsNetworkAllowed: true,
				},
				wantPairs: [][2]string{
					{"--sandbox", "workspace-write"},
					{"--ask-for-approval", "on-request"},
					{"-c", "sandbox_workspace_write.network_access=true"},
				},
				forbid: []string{`approval_policy="never"`},
			},
			{
				name: "explicit_sandbox_and_approval_override_both",
				opts: []Option{WithSandbox("danger-full-access"), WithApproval("untrusted")},
				policy: &bossanovav1.UnattendedPermissionPolicy{
					WorktreePath:     "/tmp/wt",
					IsNetworkAllowed: true,
				},
				wantPairs: [][2]string{
					{"--sandbox", "danger-full-access"},
					{"--ask-for-approval", "untrusted"},
					{"-c", "sandbox_workspace_write.network_access=true"},
				},
				forbid: []string{"workspace-write", `approval_policy="never"`},
			},
		}

		for _, tt := range tests {
			t.Run(tt.name, func(t *testing.T) {
				argv := captureHeadlessCodexArgv(t, tt.opts, tt.policy)
				assertCodexPolicyArgv(t, argv, tt.wantPairs, tt.wantFlags, tt.forbid)
			})
		}
	})

	t.Run("headless_resume", func(t *testing.T) {
		resumeID := "019e068c-9ade-7cf0-be8c-3b21977576d0"
		tests := []struct {
			name        string
			opts        []Option
			policy      *bossanovav1.UnattendedPermissionPolicy
			wantPairs   [][2]string
			wantFlags   []string
			forbid      []string
			wantSandbox string
		}{
			{
				name: "policy_present_places_sandbox_before_resume",
				policy: &bossanovav1.UnattendedPermissionPolicy{
					WorktreePath:     "/tmp/wt",
					IsNetworkAllowed: true,
				},
				wantPairs: [][2]string{
					{"--sandbox", "workspace-write"},
					{"-c", `approval_policy="never"`},
					{"-c", "sandbox_workspace_write.network_access=true"},
					{"resume", resumeID},
				},
				forbid:      []string{"--ask-for-approval", "--dangerously-bypass-approvals-and-sandbox"},
				wantSandbox: "workspace-write",
			},
			{
				name: "explicit_sandbox_places_sandbox_before_resume",
				opts: []Option{WithSandbox("read-only")},
				policy: &bossanovav1.UnattendedPermissionPolicy{
					WorktreePath:     "/tmp/wt",
					IsNetworkAllowed: false,
				},
				wantPairs: [][2]string{
					{"--sandbox", "read-only"},
					{"-c", `approval_policy="never"`},
					{"resume", resumeID},
				},
				forbid:      []string{"workspace-write", "--ask-for-approval", "sandbox_workspace_write.network_access=true"},
				wantSandbox: "read-only",
			},
			{
				name: "bypass_on_omits_sandbox_on_resume",
				opts: []Option{WithDangerouslyBypassApprovalsAndSandbox(true)},
				policy: &bossanovav1.UnattendedPermissionPolicy{
					WorktreePath:     "/tmp/wt",
					IsNetworkAllowed: true,
				},
				wantPairs: [][2]string{{"resume", resumeID}},
				wantFlags: []string{"--dangerously-bypass-approvals-and-sandbox"},
				forbid:    []string{"--sandbox", "--ask-for-approval", `approval_policy="never"`, "sandbox_workspace_write.network_access=true"},
			},
		}

		for _, tt := range tests {
			t.Run(tt.name, func(t *testing.T) {
				argv := captureHeadlessCodexRunArgv(t, tt.opts, tt.policy, &resumeID)
				assertCodexPolicyArgv(t, argv, tt.wantPairs, tt.wantFlags, tt.forbid)
				execIdx := slices.Index(argv, "exec")
				resumeIdx := slices.Index(argv, "resume")
				if execIdx < 0 || resumeIdx <= execIdx {
					t.Fatalf("expected `exec ... resume` in %v", argv)
				}
				if tt.wantSandbox != "" {
					sandboxIdx := findPairIndex(argv, "--sandbox", tt.wantSandbox)
					if sandboxIdx <= execIdx || sandboxIdx >= resumeIdx {
						t.Errorf("expected `--sandbox %s` between `exec` (%d) and `resume` (%d), got index %d in %v",
							tt.wantSandbox, execIdx, resumeIdx, sandboxIdx, argv)
					}
				}
			})
		}
	})

	t.Run("interactive", func(t *testing.T) {
		tests := []struct {
			name      string
			nilRunner bool
			opts      []Option
			policy    *bossanovav1.UnattendedPermissionPolicy
			wantPairs [][2]string
			wantFlags []string
			forbid    []string
		}{
			{
				name: "policy_present_network_allowed",
				policy: &bossanovav1.UnattendedPermissionPolicy{
					WorktreePath:     "/tmp/wt",
					IsNetworkAllowed: true,
				},
				wantPairs: [][2]string{
					{"--sandbox", "workspace-write"},
					{"--ask-for-approval", "never"},
					{"-c", "sandbox_workspace_write.network_access=true"},
				},
				forbid: []string{`approval_policy="never"`, "--dangerously-bypass-approvals-and-sandbox"},
			},
			{
				name: "policy_present_network_disallowed",
				policy: &bossanovav1.UnattendedPermissionPolicy{
					WorktreePath:     "/tmp/wt",
					IsNetworkAllowed: false,
				},
				wantPairs: [][2]string{
					{"--sandbox", "workspace-write"},
					{"--ask-for-approval", "never"},
				},
				forbid: []string{`approval_policy="never"`, "sandbox_workspace_write.network_access=true", "--dangerously-bypass-approvals-and-sandbox"},
			},
			{
				name:      "policy_present_nil_runner",
				nilRunner: true,
				policy: &bossanovav1.UnattendedPermissionPolicy{
					WorktreePath:     "/tmp/wt",
					IsNetworkAllowed: true,
				},
				wantPairs: [][2]string{
					{"--sandbox", "workspace-write"},
					{"--ask-for-approval", "never"},
					{"-c", "sandbox_workspace_write.network_access=true"},
				},
				forbid: []string{"--dangerously-bypass-approvals-and-sandbox"},
			},
			{
				name:   "policy_absent_unchanged",
				policy: nil,
				forbid: []string{"--sandbox", "--ask-for-approval", `approval_policy="never"`, "sandbox_workspace_write.network_access=true", "--dangerously-bypass-approvals-and-sandbox"},
			},
			{
				name: "policy_present_bypass_on_omits_policy",
				opts: []Option{WithDangerouslyBypassApprovalsAndSandbox(true)},
				policy: &bossanovav1.UnattendedPermissionPolicy{
					WorktreePath:     "/tmp/wt",
					IsNetworkAllowed: true,
				},
				wantFlags: []string{"--dangerously-bypass-approvals-and-sandbox"},
				forbid:    []string{"--sandbox", "--ask-for-approval", `approval_policy="never"`, "sandbox_workspace_write.network_access=true"},
			},
			{
				name: "explicit_sandbox_overrides_policy_sandbox",
				opts: []Option{WithSandbox("read-only")},
				policy: &bossanovav1.UnattendedPermissionPolicy{
					WorktreePath:     "/tmp/wt",
					IsNetworkAllowed: true,
				},
				wantPairs: [][2]string{
					{"--sandbox", "read-only"},
					{"--ask-for-approval", "never"},
					{"-c", "sandbox_workspace_write.network_access=true"},
				},
				forbid: []string{"workspace-write"},
			},
			{
				name: "explicit_approval_overrides_policy_approval",
				opts: []Option{WithApproval("on-request")},
				policy: &bossanovav1.UnattendedPermissionPolicy{
					WorktreePath:     "/tmp/wt",
					IsNetworkAllowed: true,
				},
				wantPairs: [][2]string{
					{"--sandbox", "workspace-write"},
					{"--ask-for-approval", "on-request"},
					{"-c", "sandbox_workspace_write.network_access=true"},
				},
				forbid: []string{"never"},
			},
			{
				name: "explicit_sandbox_and_approval_override_both",
				opts: []Option{WithSandbox("danger-full-access"), WithApproval("untrusted")},
				policy: &bossanovav1.UnattendedPermissionPolicy{
					WorktreePath:     "/tmp/wt",
					IsNetworkAllowed: true,
				},
				wantPairs: [][2]string{
					{"--sandbox", "danger-full-access"},
					{"--ask-for-approval", "untrusted"},
					{"-c", "sandbox_workspace_write.network_access=true"},
				},
				forbid: []string{"workspace-write", "never"},
			},
		}

		for _, tt := range tests {
			t.Run(tt.name, func(t *testing.T) {
				var srv *Server
				if tt.nilRunner {
					srv = &Server{}
				} else {
					srv = newTestServer(t, tt.opts...)
				}
				resp, err := srv.BuildInteractiveCommand(context.Background(), &bossanovav1.BuildInteractiveCommandRequest{
					SessionId:                  "sid-interactive",
					WorktreePath:               t.TempDir(),
					InitialPrompt:              "run checks",
					UnattendedPermissionPolicy: tt.policy,
				})
				if err != nil {
					t.Fatalf("BuildInteractiveCommand: %v", err)
				}
				argv := resp.GetArgv()
				assertCodexPolicyArgv(t, argv, tt.wantPairs, tt.wantFlags, tt.forbid)
				// Ensure all policy flags precede the `--` prompt separator.
				sepIdx := slices.Index(argv, "--")
				if sepIdx < 0 {
					t.Fatalf("expected -- separator before prompt in %v", argv)
				}
				for _, pair := range tt.wantPairs {
					idx := findPairIndex(argv, pair[0], pair[1])
					if idx >= sepIdx {
						t.Errorf("pair %q %q landed at %d after `--` (%d) in %v", pair[0], pair[1], idx, sepIdx, argv)
					}
				}
			})
		}
	})
}

// TestUnattendedPolicyArgv_Sessions_AC267802 pins BOS-2678 AC267802: with
// dangerous mode off, an unattended Codex launch runs in the workspace-write
// sandbox with network access and no approval prompts, and explicit user
// sandbox or approval settings win for their own flag.
func TestUnattendedPolicyArgv_Sessions_AC267802(t *testing.T) {
	policy := &bossanovav1.UnattendedPermissionPolicy{
		WorktreePath:     "/tmp/wt",
		IsNetworkAllowed: true,
	}

	// Default unattended policy (headless + interactive).
	headlessArgv := captureHeadlessCodexArgv(t, nil, policy)
	assertCodexPolicyArgv(t, headlessArgv,
		[][2]string{
			{"--sandbox", "workspace-write"},
			{"-c", `approval_policy="never"`},
			{"-c", "sandbox_workspace_write.network_access=true"},
		},
		nil,
		[]string{"--ask-for-approval", "--dangerously-bypass-approvals-and-sandbox"},
	)

	srv := newTestServer(t)
	intResp, err := srv.BuildInteractiveCommand(context.Background(), &bossanovav1.BuildInteractiveCommandRequest{
		SessionId:                  "ac267802-interactive",
		WorktreePath:               t.TempDir(),
		UnattendedPermissionPolicy: policy,
	})
	if err != nil {
		t.Fatalf("BuildInteractiveCommand: %v", err)
	}
	assertCodexPolicyArgv(t, intResp.GetArgv(),
		[][2]string{
			{"--sandbox", "workspace-write"},
			{"--ask-for-approval", "never"},
			{"-c", "sandbox_workspace_write.network_access=true"},
		},
		nil,
		[]string{"--dangerously-bypass-approvals-and-sandbox"},
	)

	// Explicit user sandbox overrides policy sandbox; policy approval and network remain.
	headlessCustomSandbox := captureHeadlessCodexArgv(t, []Option{WithSandbox("read-only")}, policy)
	assertCodexPolicyArgv(t, headlessCustomSandbox,
		[][2]string{
			{"--sandbox", "read-only"},
			{"-c", `approval_policy="never"`},
			{"-c", "sandbox_workspace_write.network_access=true"},
		},
		nil,
		[]string{"workspace-write"},
	)

	// Explicit user approval overrides policy approval; policy sandbox and network remain.
	srvCustomApproval := newTestServer(t, WithApproval("on-request"))
	intCustomApproval, err := srvCustomApproval.BuildInteractiveCommand(context.Background(), &bossanovav1.BuildInteractiveCommandRequest{
		SessionId:                  "ac267802-custom-approval",
		WorktreePath:               t.TempDir(),
		UnattendedPermissionPolicy: policy,
	})
	if err != nil {
		t.Fatalf("BuildInteractiveCommand (custom approval): %v", err)
	}
	assertCodexPolicyArgv(t, intCustomApproval.GetArgv(),
		[][2]string{
			{"--sandbox", "workspace-write"},
			{"--ask-for-approval", "on-request"},
			{"-c", "sandbox_workspace_write.network_access=true"},
		},
		nil,
		[]string{"never"},
	)
}

func captureHeadlessCodexArgv(t *testing.T, opts []Option, policy *bossanovav1.UnattendedPermissionPolicy) []string {
	t.Helper()
	return captureHeadlessCodexRunArgv(t, opts, policy, nil)
}

func captureHeadlessCodexRunArgv(t *testing.T, opts []Option, policy *bossanovav1.UnattendedPermissionPolicy, resumeID *string) []string {
	t.Helper()
	var mu sync.Mutex
	var captured []string
	factory := func(ctx context.Context, name string, args ...string) *exec.Cmd {
		mu.Lock()
		captured = append([]string{name}, args...)
		mu.Unlock()
		return exec.CommandContext(ctx, "/bin/sh", "-c", `printf '{"type":"thread.started","thread_id":"00000000-0000-0000-0000-000000000001"}\n'`)
	}
	allOpts := append([]Option{WithCommandFactory(factory)}, opts...)
	srv := newServer(nil, zerolog.Nop(), allOpts...)
	resp, err := srv.StartAgentRun(context.Background(), &bossanovav1.StartAgentRunRequest{
		WorkDir:                    t.TempDir(),
		Plan:                       "check status",
		ResumeId:                   resumeID,
		SessionId:                  "sid-headless",
		LogPath:                    filepath.Join(t.TempDir(), "codex.log"),
		UnattendedPermissionPolicy: policy,
	})
	if err != nil {
		t.Fatalf("StartAgentRun: %v", err)
	}
	t.Cleanup(func() { _ = srv.runner.Stop(resp.GetSessionId()) })
	mu.Lock()
	defer mu.Unlock()
	return slices.Clone(captured)
}

func assertCodexPolicyArgv(t *testing.T, argv []string, wantPairs [][2]string, wantFlags, forbid []string) {
	t.Helper()
	for _, pair := range wantPairs {
		if findPairIndex(argv, pair[0], pair[1]) < 0 {
			t.Errorf("argv %v missing pair [%q %q]", argv, pair[0], pair[1])
		}
	}
	for _, flag := range wantFlags {
		if !slices.Contains(argv, flag) {
			t.Errorf("argv %v missing flag %q", argv, flag)
		}
	}
	for _, banned := range forbid {
		if slices.Contains(argv, banned) {
			t.Errorf("argv %v unexpectedly contains %q", argv, banned)
		}
	}
}

func findPairIndex(argv []string, first, second string) int {
	for i := 0; i < len(argv)-1; i++ {
		if argv[i] == first && argv[i+1] == second {
			return i
		}
	}
	return -1
}
