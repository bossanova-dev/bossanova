package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/rs/zerolog"

	bossanovav1 "github.com/recurser/bossalib/gen/bossanova/v1"
)

func TestUnattendedPolicyArgv(t *testing.T) {
	t.Run("headless", func(t *testing.T) {
		tests := []struct {
			name        string
			trusted     bool
			bypass      bool
			withPolicy  bool
			logInWT     bool
			wantPolicy  bool
			wantIsolate bool
		}{
			{
				name:        "policy_present_trusted_bypass_off",
				trusted:     true,
				bypass:      false,
				withPolicy:  true,
				wantPolicy:  true,
				wantIsolate: false,
			},
			{
				name:        "policy_present_untrusted_bypass_off",
				trusted:     false,
				bypass:      false,
				withPolicy:  true,
				wantPolicy:  true,
				wantIsolate: true,
			},
			{
				name:        "policy_present_bypass_on_omits_policy",
				trusted:     false,
				bypass:      true,
				withPolicy:  true,
				wantPolicy:  false,
				wantIsolate: true,
			},
			{
				name:        "policy_absent_trusted_unchanged",
				trusted:     true,
				bypass:      false,
				withPolicy:  false,
				wantPolicy:  false,
				wantIsolate: false,
			},
			{
				name:        "policy_absent_untrusted_unchanged",
				trusted:     false,
				bypass:      false,
				withPolicy:  false,
				wantPolicy:  false,
				wantIsolate: true,
			},
			{
				name:        "log_path_inside_worktree_falls_back_outside_worktree",
				trusted:     true,
				bypass:      false,
				withPolicy:  true,
				logInWT:     true,
				wantPolicy:  true,
				wantIsolate: false,
			},
		}

		for _, tt := range tests {
			t.Run(tt.name, func(t *testing.T) {
				worktree := t.TempDir()
				logDir := t.TempDir()
				logPath := filepath.Join(logDir, "claude.log")
				if tt.logInWT {
					logPath = filepath.Join(worktree, "claude.log")
				}

				var launchArgs []string
				pr, pw, err := os.Pipe()
				if err != nil {
					t.Fatalf("os.Pipe: %v", err)
				}
				t.Cleanup(func() {
					_ = pw.Close()
					_ = pr.Close()
				})
				factory := func(ctx context.Context, _ string, args ...string) *exec.Cmd {
					if slices.Contains(args, "--help") {
						return exec.CommandContext(ctx, "/bin/sh", "-c", "printf '%s' '--strict-mcp-config --setting-sources'")
					}
					launchArgs = slices.Clone(args)
					cmd := exec.CommandContext(ctx, "/bin/sh", "-c", "cat >/dev/null; exec cat <&3")
					cmd.ExtraFiles = []*os.File{pr}
					return cmd
				}
				opts := []RunnerOption{WithCommandFactory(factory)}
				if tt.bypass {
					opts = append(opts, WithDangerouslySkipPermissions(true))
				}
				srv := newServer(nil, zerolog.Nop(), opts...)

				req := &bossanovav1.StartAgentRunRequest{
					WorkDir:                  worktree,
					Plan:                     "plan",
					SessionId:                "sid-headless",
					LogPath:                  logPath,
					ShouldTrustProjectConfig: tt.trusted,
				}
				if tt.withPolicy {
					req.UnattendedPermissionPolicy = &bossanovav1.UnattendedPermissionPolicy{
						WorktreePath:           worktree,
						AllowedCommandPrefixes: []string{"git", "make", "git", "bad cmd", "../evil"},
						IsNetworkAllowed:       true,
					}
				}

				resp, err := srv.StartAgentRun(context.Background(), req)
				if err != nil {
					t.Fatalf("StartAgentRun: %v", err)
				}
				ch, err := srv.runner.Subscribe(context.Background(), resp.GetSessionId())
				if err != nil {
					t.Fatalf("Subscribe: %v", err)
				}
				t.Cleanup(func() { _ = srv.runner.Stop(resp.GetSessionId()) })

				assertPolicyArgvAndFile(t, launchArgs, worktree, logDir, tt.logInWT, tt.wantPolicy, tt.bypass, tt.wantIsolate, []string{"git", "make"})

				settingsPath := flagValue(launchArgs, "--settings")
				_ = pw.Close()
				for range ch {
				}
				if settingsPath != "" {
					if _, statErr := os.Stat(settingsPath); !errors.Is(statErr, os.ErrNotExist) {
						t.Fatalf("headless policy file %q survived process exit: stat err=%v", settingsPath, statErr)
					}
				}
			})
		}
	})

	t.Run("interactive", func(t *testing.T) {
		tests := []struct {
			name        string
			trust       bossanovav1.ProjectTrustDecision
			bypass      bool
			withPolicy  bool
			emptyLog    bool
			logInWT     bool
			wantPolicy  bool
			wantIsolate bool
		}{
			{
				name:        "policy_present_trusted_bypass_off",
				trust:       bossanovav1.ProjectTrustDecision_PROJECT_TRUST_DECISION_GRANT,
				bypass:      false,
				withPolicy:  true,
				wantPolicy:  true,
				wantIsolate: false,
			},
			{
				name:        "policy_present_untrusted_bypass_off",
				trust:       bossanovav1.ProjectTrustDecision_PROJECT_TRUST_DECISION_REVOKE,
				bypass:      false,
				withPolicy:  true,
				wantPolicy:  true,
				wantIsolate: true,
			},
			{
				name:        "policy_present_bypass_on_omits_policy",
				trust:       bossanovav1.ProjectTrustDecision_PROJECT_TRUST_DECISION_REVOKE,
				bypass:      true,
				withPolicy:  true,
				wantPolicy:  false,
				wantIsolate: true,
			},
			{
				name:        "policy_absent_trusted_unchanged",
				trust:       bossanovav1.ProjectTrustDecision_PROJECT_TRUST_DECISION_GRANT,
				bypass:      false,
				withPolicy:  false,
				wantPolicy:  false,
				wantIsolate: false,
			},
			{
				name:        "policy_absent_untrusted_unchanged",
				trust:       bossanovav1.ProjectTrustDecision_PROJECT_TRUST_DECISION_REVOKE,
				bypass:      false,
				withPolicy:  false,
				wantPolicy:  false,
				wantIsolate: true,
			},
			{
				name:        "empty_log_path_falls_back_outside_worktree",
				trust:       bossanovav1.ProjectTrustDecision_PROJECT_TRUST_DECISION_REVOKE,
				bypass:      false,
				withPolicy:  true,
				emptyLog:    true,
				wantPolicy:  true,
				wantIsolate: true,
			},
			{
				name:        "log_path_inside_worktree_falls_back_outside_worktree",
				trust:       bossanovav1.ProjectTrustDecision_PROJECT_TRUST_DECISION_GRANT,
				bypass:      false,
				withPolicy:  true,
				logInWT:     true,
				wantPolicy:  true,
				wantIsolate: false,
			},
		}

		for _, tt := range tests {
			t.Run(tt.name, func(t *testing.T) {
				worktree := t.TempDir()
				logDir := t.TempDir()
				logPath := filepath.Join(logDir, "claude.log")
				if tt.emptyLog {
					logPath = ""
				} else if tt.logInWT {
					logPath = filepath.Join(worktree, "claude.log")
				}

				opts := []RunnerOption{WithCommandFactory(fakeClaude(t, "true"))}
				if tt.bypass {
					opts = append(opts, WithDangerouslySkipPermissions(true))
				}
				srv := newServer(nil, zerolog.Nop(), opts...)

				req := &bossanovav1.BuildInteractiveCommandRequest{
					WorktreePath: worktree,
					SessionId:    "sid-interactive",
					LogPath:      logPath,
					ProjectTrust: tt.trust,
				}
				if tt.withPolicy {
					req.UnattendedPermissionPolicy = &bossanovav1.UnattendedPermissionPolicy{
						WorktreePath:           worktree,
						AllowedCommandPrefixes: []string{"git", "gh", "go"},
						IsNetworkAllowed:       true,
					}
				}

				resp, err := srv.BuildInteractiveCommand(context.Background(), req)
				if err != nil {
					t.Fatalf("BuildInteractiveCommand: %v", err)
				}

				assertPolicyArgvAndFile(t, resp.GetArgv(), worktree, logDir, tt.emptyLog || tt.logInWT, tt.wantPolicy, tt.bypass, tt.wantIsolate, []string{"git", "gh", "go"})
			})
		}
	})

	t.Run("concurrent_runs_produce_distinct_files", func(t *testing.T) {
		worktree := t.TempDir()
		logDir := t.TempDir()
		logPath := filepath.Join(logDir, "claude.log")
		srv := newServer(nil, zerolog.Nop())

		policy := &bossanovav1.UnattendedPermissionPolicy{
			WorktreePath:           worktree,
			AllowedCommandPrefixes: []string{"git"},
			IsNetworkAllowed:       true,
		}
		resp1, err := srv.BuildInteractiveCommand(context.Background(), &bossanovav1.BuildInteractiveCommandRequest{
			WorktreePath:               worktree,
			SessionId:                  "sid-1",
			LogPath:                    logPath,
			ProjectTrust:               bossanovav1.ProjectTrustDecision_PROJECT_TRUST_DECISION_GRANT,
			UnattendedPermissionPolicy: policy,
		})
		if err != nil {
			t.Fatalf("first BuildInteractiveCommand: %v", err)
		}
		resp2, err := srv.BuildInteractiveCommand(context.Background(), &bossanovav1.BuildInteractiveCommandRequest{
			WorktreePath:               worktree,
			SessionId:                  "sid-2",
			LogPath:                    logPath,
			ProjectTrust:               bossanovav1.ProjectTrustDecision_PROJECT_TRUST_DECISION_GRANT,
			UnattendedPermissionPolicy: policy,
		})
		if err != nil {
			t.Fatalf("second BuildInteractiveCommand: %v", err)
		}
		p1 := flagValue(resp1.GetArgv(), "--settings")
		p2 := flagValue(resp2.GetArgv(), "--settings")
		if p1 == "" || p2 == "" || p1 == p2 {
			t.Fatalf("expected distinct settings files, got %q and %q", p1, p2)
		}
	})
}

// TestUnattendedPolicyArgv_Sessions_AC267801 pins the BOS-2678 acceptance
// criterion for unattended Claude launches (including untrusted repos) with
// dangerous mode off.
func TestUnattendedPolicyArgv_Sessions_AC267801(t *testing.T) {
	worktree := t.TempDir()
	logDir := t.TempDir()
	logPath := filepath.Join(logDir, "claude.log")

	var launchArgs []string
	pr, pw, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	t.Cleanup(func() {
		_ = pw.Close()
		_ = pr.Close()
	})
	factory := func(ctx context.Context, _ string, args ...string) *exec.Cmd {
		if slices.Contains(args, "--help") {
			return exec.CommandContext(ctx, "/bin/sh", "-c", "printf '%s' '--strict-mcp-config --setting-sources'")
		}
		launchArgs = slices.Clone(args)
		cmd := exec.CommandContext(ctx, "/bin/sh", "-c", "cat >/dev/null; exec cat <&3")
		cmd.ExtraFiles = []*os.File{pr}
		return cmd
	}
	srv := newServer(nil, zerolog.Nop(), WithCommandFactory(factory))

	policy := &bossanovav1.UnattendedPermissionPolicy{
		WorktreePath:           worktree,
		AllowedCommandPrefixes: []string{"git", "gh", "boss", "node", "make"},
		IsNetworkAllowed:       true,
	}

	// Headless untrusted run.
	runResp, err := srv.StartAgentRun(context.Background(), &bossanovav1.StartAgentRunRequest{
		WorkDir:                    worktree,
		Plan:                       "check status and edit",
		SessionId:                  "ac267801-headless",
		LogPath:                    logPath,
		ShouldTrustProjectConfig:   false,
		UnattendedPermissionPolicy: policy,
	})
	if err != nil {
		t.Fatalf("StartAgentRun: %v", err)
	}
	ch, err := srv.runner.Subscribe(context.Background(), runResp.GetSessionId())
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	t.Cleanup(func() { _ = srv.runner.Stop(runResp.GetSessionId()) })
	assertPolicyArgvAndFile(t, launchArgs, worktree, logDir, false, true, false, true, policy.AllowedCommandPrefixes)
	_ = pw.Close()
	for range ch {
	}

	// Interactive untrusted run.
	intResp, err := srv.BuildInteractiveCommand(context.Background(), &bossanovav1.BuildInteractiveCommandRequest{
		WorktreePath:               worktree,
		SessionId:                  "ac267801-interactive",
		LogPath:                    logPath,
		ProjectTrust:               bossanovav1.ProjectTrustDecision_PROJECT_TRUST_DECISION_REVOKE,
		UnattendedPermissionPolicy: policy,
	})
	if err != nil {
		t.Fatalf("BuildInteractiveCommand: %v", err)
	}
	assertPolicyArgvAndFile(t, intResp.GetArgv(), worktree, logDir, false, true, false, true, policy.AllowedCommandPrefixes)
}

func assertPolicyArgvAndFile(t *testing.T, argv []string, worktree, logDir string, usedFallbackDir, wantPolicy, wantBypass, wantIsolate bool, wantPrefixes []string) {
	t.Helper()

	if got := slices.Contains(argv, "--dangerously-skip-permissions"); got != wantBypass {
		t.Fatalf("--dangerously-skip-permissions present=%v, want %v (argv=%v)", got, wantBypass, argv)
	}
	for _, flag := range []string{"--strict-mcp-config", "--setting-sources"} {
		if got := slices.Contains(argv, flag); got != wantIsolate {
			t.Fatalf("%s present=%v, want %v (argv=%v)", flag, got, wantIsolate, argv)
		}
	}
	if wantIsolate {
		if got := flagValue(argv, "--setting-sources"); got != "user" {
			t.Fatalf("--setting-sources = %q, want user (argv=%v)", got, argv)
		}
	}

	settingsPath := flagValue(argv, "--settings")
	permMode := flagValue(argv, "--permission-mode")
	if !wantPolicy {
		if settingsPath != "" || permMode != "" {
			t.Fatalf("expected no policy flags, got --settings=%q --permission-mode=%q (argv=%v)", settingsPath, permMode, argv)
		}
		// Ensure no policy file was written into logDir or worktree.
		for _, dir := range []string{logDir, worktree} {
			entries, err := os.ReadDir(dir)
			if err != nil {
				t.Fatalf("read dir %s: %v", dir, err)
			}
			for _, e := range entries {
				if strings.HasPrefix(e.Name(), "claude-policy-") {
					t.Fatalf("unexpected policy file %s written in %s", e.Name(), dir)
				}
			}
		}
		return
	}

	if settingsPath == "" {
		t.Fatalf("missing --settings in argv %v", argv)
	}
	if usedFallbackDir {
		t.Cleanup(func() { _ = os.Remove(settingsPath) })
	}
	if permMode != "acceptEdits" {
		t.Fatalf("--permission-mode = %q, want acceptEdits (argv=%v)", permMode, argv)
	}

	if isPathWithinWorktree(filepath.Dir(settingsPath), worktree) {
		t.Fatalf("settings file %q must live outside worktree %q", settingsPath, worktree)
	}
	if !usedFallbackDir && filepath.Clean(filepath.Dir(settingsPath)) != filepath.Clean(logDir) {
		t.Fatalf("settings file %q should live beside log in %q", settingsPath, logDir)
	}

	info, err := os.Stat(settingsPath)
	if err != nil {
		t.Fatalf("stat settings file %q: %v", settingsPath, err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("settings file mode = %04o, want 0600", perm)
	}

	data, err := os.ReadFile(settingsPath)
	if err != nil {
		t.Fatalf("read settings file %q: %v", settingsPath, err)
	}
	var parsed claudePolicySettings
	if err := json.Unmarshal(data, &parsed); err != nil {
		t.Fatalf("unmarshal settings JSON %q: %v", string(data), err)
	}

	wantAllow := make([]string, 0, len(wantPrefixes)+4)
	for _, p := range wantPrefixes {
		wantAllow = append(wantAllow, "Bash("+p+":*)")
	}
	for _, root := range policyWorktreeRoots(worktree) {
		trimmed := strings.TrimPrefix(filepath.ToSlash(root), "/")
		wantAllow = append(wantAllow, "Edit(//"+trimmed+"/**)", "Write(//"+trimmed+"/**)")
	}
	if !slices.Equal(parsed.Permissions.Allow, wantAllow) {
		t.Fatalf("permissions.allow =\n got: %v\nwant: %v", parsed.Permissions.Allow, wantAllow)
	}
	for _, rule := range parsed.Permissions.Allow {
		if strings.Contains(rule, "///") {
			t.Fatalf("rule %q contains triple slash", rule)
		}
	}
}

func flagValue(argv []string, flag string) string {
	for i := 0; i < len(argv)-1; i++ {
		if argv[i] == flag {
			return argv[i+1]
		}
	}
	return ""
}

func TestUnattendedPolicySettingsCleanup(t *testing.T) {
	t.Run("headless_error_exit_removes_policy_file", func(t *testing.T) {
		worktree := t.TempDir()
		logDir := t.TempDir()
		var launchArgs []string
		factory := func(ctx context.Context, _ string, args ...string) *exec.Cmd {
			launchArgs = slices.Clone(args)
			return exec.CommandContext(ctx, "/bin/sh", "-c", "cat >/dev/null; exit 1")
		}
		srv := newServer(nil, zerolog.Nop(), WithCommandFactory(factory))
		resp, err := srv.StartAgentRun(context.Background(), &bossanovav1.StartAgentRunRequest{
			WorkDir:                  worktree,
			Plan:                     "plan",
			SessionId:                "sid-err-exit",
			LogPath:                  filepath.Join(logDir, "claude.log"),
			ShouldTrustProjectConfig: true,
			UnattendedPermissionPolicy: &bossanovav1.UnattendedPermissionPolicy{
				WorktreePath:           worktree,
				AllowedCommandPrefixes: []string{"git"},
				IsNetworkAllowed:       true,
			},
		})
		if err != nil {
			t.Fatalf("StartAgentRun: %v", err)
		}
		ch, err := srv.runner.Subscribe(context.Background(), resp.GetSessionId())
		if err != nil {
			t.Fatalf("Subscribe: %v", err)
		}
		for range ch {
		}
		settingsPath := flagValue(launchArgs, "--settings")
		if settingsPath == "" {
			t.Fatalf("expected --settings in argv %v", launchArgs)
		}
		if _, err := os.Stat(settingsPath); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("policy file %q survived error exit: stat err=%v", settingsPath, err)
		}
	})

	t.Run("headless_start_failure_removes_owned_fallback_dir", func(t *testing.T) {
		prevDefault := policySettingsDefaultDir
		policySettingsDefaultDir = func() (string, error) { return "", errors.New("no cache dir") }
		t.Cleanup(func() { policySettingsDefaultDir = prevDefault })

		worktree := t.TempDir()
		var launchArgs []string
		factory := func(ctx context.Context, _ string, args ...string) *exec.Cmd {
			launchArgs = slices.Clone(args)
			return exec.CommandContext(ctx, "/nonexistent-bossd-claude-binary")
		}
		srv := newServer(nil, zerolog.Nop(), WithCommandFactory(factory))
		_, err := srv.StartAgentRun(context.Background(), &bossanovav1.StartAgentRunRequest{
			WorkDir:                  worktree,
			Plan:                     "plan",
			SessionId:                "sid-start-fail",
			LogPath:                  filepath.Join(worktree, "claude.log"),
			ShouldTrustProjectConfig: true,
			UnattendedPermissionPolicy: &bossanovav1.UnattendedPermissionPolicy{
				WorktreePath:           worktree,
				AllowedCommandPrefixes: []string{"git"},
				IsNetworkAllowed:       true,
			},
		})
		if err == nil {
			t.Fatal("StartAgentRun succeeded for nonexistent binary")
		}
		settingsPath := flagValue(launchArgs, "--settings")
		if settingsPath == "" {
			t.Fatalf("expected --settings in argv %v", launchArgs)
		}
		if _, err := os.Stat(filepath.Dir(settingsPath)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("owned fallback dir %q survived start failure: stat err=%v", filepath.Dir(settingsPath), err)
		}
	})

	t.Run("interactive_wrap_initial_input_failure_cleans_up_policy_file", func(t *testing.T) {
		worktree := t.TempDir()
		logDir := t.TempDir()
		srv := newServer(nil, zerolog.Nop(), WithLoginShell("/bin/sh"))
		_, err := srv.BuildInteractiveCommand(context.Background(), &bossanovav1.BuildInteractiveCommandRequest{
			WorktreePath:     worktree,
			SessionId:        "sid-wrap-fail",
			LogPath:          filepath.Join(logDir, "claude.log"),
			InitialPrompt:    "run the full test suite",
			InitialInputPath: filepath.Join(t.TempDir(), "missing-dir", "prompt.initial-input"),
			ProjectTrust:     bossanovav1.ProjectTrustDecision_PROJECT_TRUST_DECISION_GRANT,
			UnattendedPermissionPolicy: &bossanovav1.UnattendedPermissionPolicy{
				WorktreePath:           worktree,
				AllowedCommandPrefixes: []string{"git"},
				IsNetworkAllowed:       true,
			},
		})
		if err == nil {
			t.Fatal("BuildInteractiveCommand succeeded with unwritable InitialInputPath")
		}
		entries, readErr := os.ReadDir(logDir)
		if readErr != nil {
			t.Fatalf("ReadDir(%q): %v", logDir, readErr)
		}
		for _, e := range entries {
			if strings.HasPrefix(e.Name(), policySettingsFilePrefix) {
				t.Fatalf("policy file %q leaked after WrapInitialInput failure", e.Name())
			}
		}
	})

	t.Run("fallback_reuses_default_dir_instead_of_leaking_temp_dirs", func(t *testing.T) {
		worktree := t.TempDir()
		wantDefault, err := policySettingsDefaultDir()
		if err != nil {
			t.Fatalf("policySettingsDefaultDir: %v", err)
		}
		srv := newServer(nil, zerolog.Nop())
		policy := &bossanovav1.UnattendedPermissionPolicy{
			WorktreePath:           worktree,
			AllowedCommandPrefixes: []string{"git"},
			IsNetworkAllowed:       true,
		}
		r1, err := srv.BuildInteractiveCommand(context.Background(), &bossanovav1.BuildInteractiveCommandRequest{
			WorktreePath:               worktree,
			SessionId:                  "sid-fb-1",
			LogPath:                    "",
			ProjectTrust:               bossanovav1.ProjectTrustDecision_PROJECT_TRUST_DECISION_GRANT,
			UnattendedPermissionPolicy: policy,
		})
		if err != nil {
			t.Fatalf("first BuildInteractiveCommand: %v", err)
		}
		r2, err := srv.BuildInteractiveCommand(context.Background(), &bossanovav1.BuildInteractiveCommandRequest{
			WorktreePath:               worktree,
			SessionId:                  "sid-fb-2",
			LogPath:                    filepath.Join(worktree, "claude.log"),
			ProjectTrust:               bossanovav1.ProjectTrustDecision_PROJECT_TRUST_DECISION_GRANT,
			UnattendedPermissionPolicy: policy,
		})
		if err != nil {
			t.Fatalf("second BuildInteractiveCommand: %v", err)
		}
		p1 := flagValue(r1.GetArgv(), "--settings")
		p2 := flagValue(r2.GetArgv(), "--settings")
		t.Cleanup(func() {
			_ = os.Remove(p1)
			_ = os.Remove(p2)
		})
		if filepath.Clean(filepath.Dir(p1)) != filepath.Clean(wantDefault) || filepath.Clean(filepath.Dir(p2)) != filepath.Clean(wantDefault) {
			t.Fatalf("fallback dirs = (%q, %q), want shared default dir %q", filepath.Dir(p1), filepath.Dir(p2), wantDefault)
		}
	})

	t.Run("sweep_stale_policy_settings_runs_on_write_and_keeps_fresh_and_other_files", func(t *testing.T) {
		worktree := t.TempDir()
		logDir := t.TempDir()
		now := time.Now()
		writeAged := func(name string, age time.Duration) string {
			p := filepath.Join(logDir, name)
			if err := os.WriteFile(p, []byte("{}"), 0o600); err != nil {
				t.Fatal(err)
			}
			ts := now.Add(-age)
			if err := os.Chtimes(p, ts, ts); err != nil {
				t.Fatal(err)
			}
			return p
		}
		stale := writeAged("claude-policy-old-1234.json", 25*time.Hour)
		fresh := writeAged("claude-policy-new-5678.json", time.Hour)
		logFile := writeAged("agent-1.log", 48*time.Hour)

		written, err := writeUnattendedPolicySettings(&bossanovav1.UnattendedPermissionPolicy{
			WorktreePath:           worktree,
			AllowedCommandPrefixes: []string{"git"},
			IsNetworkAllowed:       true,
		}, worktree, filepath.Join(logDir, "claude.log"), "sid-sweep")
		if err != nil {
			t.Fatalf("writeUnattendedPolicySettings: %v", err)
		}
		t.Cleanup(written.cleanup)

		if _, err := os.Stat(stale); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("stale policy file survived write-time sweep: %v", err)
		}
		for _, keep := range []string{fresh, logFile, written.path} {
			if _, err := os.Stat(keep); err != nil {
				t.Fatalf("%s should have been preserved: %v", keep, err)
			}
		}
	})
}
