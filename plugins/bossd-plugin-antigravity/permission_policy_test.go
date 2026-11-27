package main

import (
	"context"
	"os/exec"
	"path/filepath"
	"slices"
	"sync"
	"testing"

	bossanovav1 "github.com/recurser/bossalib/gen/bossanova/v1"
)

func TestUnattendedPolicyArgv(t *testing.T) {
	policy := &bossanovav1.UnattendedPermissionPolicy{
		WorktreePath:           "/tmp/wt",
		AllowedCommandPrefixes: []string{"git", "make"},
		IsNetworkAllowed:       true,
	}

	tests := []struct {
		name    string
		opts    []Option
		policy  *bossanovav1.UnattendedPermissionPolicy
		wantSeq [][]string
		without []string
	}{
		{
			name:    "policy_present_bypass_off_no_mode_adds_mode_auto",
			policy:  policy,
			wantSeq: [][]string{{"--mode", "auto"}},
			without: []string{"--dangerously-skip-permissions"},
		},
		{
			name:    "policy_absent_bypass_off_no_mode_unchanged",
			policy:  nil,
			without: []string{"--mode", "auto", "--dangerously-skip-permissions"},
		},
		{
			name:    "policy_present_bypass_on_omits_mode_auto",
			opts:    []Option{WithDangerouslySkipPermissions(true)},
			policy:  policy,
			wantSeq: [][]string{{"--dangerously-skip-permissions"}},
			without: []string{"--mode", "auto"},
		},
		{
			name:    "policy_present_configured_accept_edits_wins",
			opts:    []Option{WithMode("accept-edits")},
			policy:  policy,
			wantSeq: [][]string{{"--mode", "accept-edits"}},
			without: []string{"auto", "--dangerously-skip-permissions"},
		},
		{
			name:    "policy_present_configured_plan_wins",
			opts:    []Option{WithMode("plan")},
			policy:  policy,
			wantSeq: [][]string{{"--mode", "plan"}},
			without: []string{"auto", "--dangerously-skip-permissions"},
		},
	}

	t.Run("headless", func(t *testing.T) {
		for _, tt := range tests {
			t.Run(tt.name, func(t *testing.T) {
				var mu sync.Mutex
				var launchArgv []string
				baseFactory := fakeAgy(`echo '` + initLine(conversationA) + `'`)
				factory := func(ctx context.Context, name string, args ...string) *exec.Cmd {
					mu.Lock()
					launchArgv = append([]string{name}, args...)
					mu.Unlock()
					return baseFactory(ctx, name, args...)
				}
				allOpts := append([]Option{WithCommandFactory(factory)}, tt.opts...)
				s, _ := newTestServer(t, allOpts...)

				resp, err := s.StartAgentRun(context.Background(), &bossanovav1.StartAgentRunRequest{
					WorkDir:                    t.TempDir(),
					Plan:                       "work",
					SessionId:                  "boss-key",
					LogPath:                    filepath.Join(t.TempDir(), "run.log"),
					UnattendedPermissionPolicy: tt.policy,
				})
				if err != nil {
					t.Fatalf("StartAgentRun: %v", err)
				}
				waitRun(t, s, resp.GetSessionId())

				mu.Lock()
				argv := slices.Clone(launchArgv)
				mu.Unlock()
				assertAgyPolicyArgv(t, argv, tt.wantSeq, tt.without)
			})
		}
	})

	t.Run("interactive", func(t *testing.T) {
		for _, tt := range tests {
			t.Run(tt.name, func(t *testing.T) {
				s, _ := newTestServer(t, tt.opts...)
				resp, err := s.BuildInteractiveCommand(context.Background(), &bossanovav1.BuildInteractiveCommandRequest{
					WorktreePath:               t.TempDir(),
					SessionId:                  "boss-chat",
					InitialPrompt:              "run checks",
					UnattendedPermissionPolicy: tt.policy,
				})
				if err != nil {
					t.Fatalf("BuildInteractiveCommand: %v", err)
				}
				assertAgyPolicyArgv(t, resp.GetArgv(), tt.wantSeq, tt.without)
			})
		}
	})
}

func assertAgyPolicyArgv(t *testing.T, argv []string, wantSeq [][]string, without []string) {
	t.Helper()
	for _, seq := range wantSeq {
		if !containsSeq(argv, seq) {
			t.Errorf("argv %q lacks %q", argv, seq)
		}
	}
	for _, absent := range without {
		if slices.Contains(argv, absent) {
			t.Errorf("argv %q unexpectedly has %q", argv, absent)
		}
	}
}
