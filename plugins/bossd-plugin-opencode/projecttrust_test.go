package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	bossanovav1 "github.com/recurser/bossalib/gen/bossanova/v1"
)

func TestProjectTrustRefusesUnverifiedLaunch(t *testing.T) {
	for _, version := range []string{"", "unknown", "1.18.3", "1.18", "1.18.4-beta", "-1.18.4"} {
		for _, decision := range []bossanovav1.ProjectTrustDecision{
			bossanovav1.ProjectTrustDecision_PROJECT_TRUST_DECISION_REVOKE,
			bossanovav1.ProjectTrustDecision_PROJECT_TRUST_DECISION_DENY,
		} {
			t.Run(version+"/"+decision.String(), func(t *testing.T) {
				assertProjectTrustRefused(t, version, decision, codes.FailedPrecondition)
			})
		}
	}
	t.Run("unknown decision", func(t *testing.T) {
		assertProjectTrustRefused(t, "1.18.4", bossanovav1.ProjectTrustDecision(99), codes.InvalidArgument)
	})
}

func assertProjectTrustRefused(t *testing.T, version string, decision bossanovav1.ProjectTrustDecision, want codes.Code) {
	t.Helper()
	dir := t.TempDir()
	srv := newServer(nil, zerolog.Nop(), WithCLIVersion(version), WithCommandFactory(func(ctx context.Context, _ string, _ ...string) *exec.Cmd {
		t.Error("refused request launched a process")
		return exec.CommandContext(ctx, "/usr/bin/true")
	}))
	_, err := srv.StartAgentRun(context.Background(), &bossanovav1.StartAgentRunRequest{
		ProjectTrust: decision, WorkDir: dir, SessionId: "refused", LogPath: filepath.Join(dir, "run.log"), ExtraEnv: hookEnv(),
	})
	if status.Code(err) != want {
		t.Errorf("StartAgentRun error = %v, want %s", err, want)
	}
	inputPath := filepath.Join(dir, "initial-input")
	_, err = srv.BuildInteractiveCommand(context.Background(), &bossanovav1.BuildInteractiveCommandRequest{
		ProjectTrust: decision, WorktreePath: dir, InitialPrompt: "launch", InitialInputPath: inputPath,
	})
	if status.Code(err) != want {
		t.Errorf("BuildInteractiveCommand error = %v, want %s", err, want)
	}
	if want == codes.FailedPrecondition && err != nil && (!strings.Contains(err.Error(), "upgrade OpenCode") || !strings.Contains(err.Error(), "boss repo update <id> --trusted")) {
		t.Errorf("refusal lacks remediation: %v", err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Errorf("refused launch wrote files: %v", entries)
	}
}

func TestStartAgentRunProjectTrust(t *testing.T) {
	for _, decision := range []bossanovav1.ProjectTrustDecision{
		bossanovav1.ProjectTrustDecision_PROJECT_TRUST_DECISION_UNSPECIFIED,
		bossanovav1.ProjectTrustDecision_PROJECT_TRUST_DECISION_GRANT,
		bossanovav1.ProjectTrustDecision_PROJECT_TRUST_DECISION_REVOKE,
		bossanovav1.ProjectTrustDecision_PROJECT_TRUST_DECISION_DENY,
	} {
		t.Run(decision.String(), func(t *testing.T) {
			trusted := decision == bossanovav1.ProjectTrustDecision_PROJECT_TRUST_DECISION_UNSPECIFIED || decision == bossanovav1.ProjectTrustDecision_PROJECT_TRUST_DECISION_GRANT
			dir := t.TempDir()
			report := filepath.Join(dir, "environment")
			overlay := hookEnv()
			overlay["OPENCODE_DISABLE_PROJECT_CONFIG"] = "0"
			overlay["OPENCODE_PURE"] = "0"
			overlay["OPENCODE_DISABLE_EXTERNAL_SKILLS"] = "0"
			var argv []string
			srv := newServer(nil, zerolog.Nop(), WithCLIVersion("1.18.4"), WithCommandFactory(func(ctx context.Context, name string, args ...string) *exec.Cmd {
				argv = append([]string{name}, args...)
				return exec.CommandContext(ctx, "/bin/sh", "-c", `printf '%s\n' "$OPENCODE_DISABLE_PROJECT_CONFIG" "$OPENCODE_PURE" "$OPENCODE_DISABLE_EXTERNAL_SKILLS" > "$1"; if [ -f "$2" ]; then printf 'hook\n' >> "$1"; fi; printf '%s\n' '{"sessionID":"trust-run"}'`, "opencode", report, questionHookPath(dir))
			}))
			cleaned := make(chan struct{})
			srv.onHookCleanup = func(string) { close(cleaned) }
			resp, err := srv.StartAgentRun(context.Background(), &bossanovav1.StartAgentRunRequest{
				ProjectTrust: decision, WorkDir: dir, Plan: "run plan", SessionId: "trust-run", LogPath: filepath.Join(dir, "run.log"), ExtraEnv: overlay,
			})
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if err := srv.runner.Wait(ctx, resp.SessionId); err != nil {
				t.Fatal(err)
			}
			if trusted {
				select {
				case <-cleaned:
				case <-ctx.Done():
					t.Fatal("hook cleanup did not finish")
				}
			}
			got, err := os.ReadFile(report)
			if err != nil {
				t.Fatal(err)
			}
			want := "1\n1\n1\n"
			if trusted {
				want = "0\n0\n0\nhook\n"
			}
			if string(got) != want {
				t.Errorf("child environment/hook = %q, want %q", got, want)
			}
			if !slices.Equal(argv, []string{"opencode", "run", "--format", "json", "--dir", dir, "--auto", "run plan"}) {
				t.Errorf("argv changed: %q", argv)
			}
			if overlay["OPENCODE_DISABLE_PROJECT_CONFIG"] != "0" || overlay["OPENCODE_PURE"] != "0" || overlay["OPENCODE_DISABLE_EXTERNAL_SKILLS"] != "0" {
				t.Error("request environment mutated")
			}
			if !trusted {
				if _, err := os.Stat(filepath.Join(dir, ".opencode")); !os.IsNotExist(err) {
					t.Errorf("untrusted run wrote .opencode: %v", err)
				}
			}
		})
	}
}

func TestBuildInteractiveCommandProjectTrust(t *testing.T) {
	for _, decision := range []bossanovav1.ProjectTrustDecision{
		bossanovav1.ProjectTrustDecision_PROJECT_TRUST_DECISION_UNSPECIFIED,
		bossanovav1.ProjectTrustDecision_PROJECT_TRUST_DECISION_GRANT,
		bossanovav1.ProjectTrustDecision_PROJECT_TRUST_DECISION_REVOKE,
		bossanovav1.ProjectTrustDecision_PROJECT_TRUST_DECISION_DENY,
	} {
		t.Run(decision.String(), func(t *testing.T) {
			srv := newServer(nil, zerolog.Nop(), WithCLIVersion("1.18.4"))
			resp, err := srv.BuildInteractiveCommand(context.Background(), &bossanovav1.BuildInteractiveCommandRequest{
				ProjectTrust: decision, SessionId: "ses_resume", Resume: true, Model: "provider/model", InitialPrompt: "run plan",
			})
			if err != nil {
				t.Fatal(err)
			}
			want := []string{"opencode", "--session", "ses_resume", "--auto", "--model", "provider/model", "--prompt", "run plan"}
			if decision == bossanovav1.ProjectTrustDecision_PROJECT_TRUST_DECISION_REVOKE || decision == bossanovav1.ProjectTrustDecision_PROJECT_TRUST_DECISION_DENY {
				want = append([]string{"env", "OPENCODE_DISABLE_PROJECT_CONFIG=1", "OPENCODE_PURE=1", "OPENCODE_DISABLE_EXTERNAL_SKILLS=1"}, want...)
			}
			if !slices.Equal(resp.Argv, want) {
				t.Errorf("argv = %q, want %q", resp.Argv, want)
			}
		})
	}
}

func TestBuildInteractiveCommandProjectTrustThroughLoginShell(t *testing.T) {
	dir := t.TempDir()
	recorder := filepath.Join(dir, "opencode")
	script := "#!/bin/sh\nprintf '%s\\n' \"$OPENCODE_DISABLE_PROJECT_CONFIG\" \"$OPENCODE_PURE\" \"$OPENCODE_DISABLE_EXTERNAL_SKILLS\" \"$@\"\n"
	if err := os.WriteFile(recorder, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	srv := newServer(nil, zerolog.Nop(), WithCLIVersion("1.18.4"), WithLoginShell("/bin/sh"))
	prompt := "run 'quoted' plan; $(false)"
	resp, err := srv.BuildInteractiveCommand(context.Background(), &bossanovav1.BuildInteractiveCommandRequest{
		ProjectTrust:  bossanovav1.ProjectTrustDecision_PROJECT_TRUST_DECISION_DENY,
		InitialPrompt: prompt, InitialInputPath: filepath.Join(dir, "prompt with spaces"),
	})
	if err != nil {
		t.Fatal(err)
	}
	// Keep the actual login-shell and env argv; substitute only the executable
	// so this exercise does not need OpenCode installed or invoke a paid model.
	argv := slices.Clone(resp.Argv)
	for i, arg := range argv {
		if arg == "opencode" {
			argv[i] = recorder
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.WaitDelay = time.Second
	out, err := cmd.Output()
	if err != nil {
		t.Fatal(err)
	}
	if want := "1\n1\n1\n--auto\n--prompt\n" + prompt + "\n"; string(out) != want {
		t.Errorf("isolated launch output = %q, want %q", out, want)
	}
}

func TestProjectIsolationSupported(t *testing.T) {
	for _, version := range []string{"1.18.4", "v1.18.4", " 1.18.5 ", "1.19.0", "2.0.0"} {
		if !projectIsolationSupported(version) {
			t.Errorf("verified stable version %q refused", version)
		}
	}
	for _, version := range []string{"0.99.9", "1.17.99", "1.18.3", "1.18", "1.18.4-beta", "v1.18.4.1", "1.18.-4", "unknown", "", "1.18.4+local"} {
		if projectIsolationSupported(version) {
			t.Errorf("unverified version %q accepted", version)
		}
	}
}
