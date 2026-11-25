package daemon

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/recurser/bossalib/daemonstate"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
)

func TestDaemonLoginShellNodeLines(t *testing.T) {
	const (
		notRecordedLine = "node (login shell): not recorded (the running daemon predates this check, or its state file could not be read)"
		skippedLine     = "node (login shell): not probed — no supported login shell captured yet (run boss once, then 'boss daemon restart')"
		remedyPhrase    = "add node's directory to daemon_path_extra and run 'boss daemon restart'"
	)

	cases := []struct {
		name       string
		metadata   daemonstate.Metadata
		err        error
		want       string
		wantRemedy bool
	}{
		{
			name: "prepended",
			metadata: daemonstate.Metadata{
				LoginShellNode: &daemonstate.LoginShellNodeProbe{
					Shell:    "/bin/zsh",
					Status:   daemonstate.LoginShellNodeStatusPrepended,
					NodePath: "/Users/dev/.nvm/versions/node/v22.12.0/bin/node",
					Dir:      "/Users/dev/.nvm/versions/node/v22.12.0/bin",
				},
			},
			want: "node (login shell): /Users/dev/.nvm/versions/node/v22.12.0/bin/node — bossd prepended /Users/dev/.nvm/versions/node/v22.12.0/bin to its PATH",
		},
		{
			name: "on_path",
			metadata: daemonstate.Metadata{
				LoginShellNode: &daemonstate.LoginShellNodeProbe{
					Shell:    "/bin/zsh",
					Status:   daemonstate.LoginShellNodeStatusOnPath,
					NodePath: "/opt/homebrew/bin/node",
					Dir:      "/opt/homebrew/bin",
				},
			},
			want: "node (login shell): /opt/homebrew/bin/node — already on the daemon's PATH",
		},
		{
			name: "not_found",
			metadata: daemonstate.Metadata{
				LoginShellNode: &daemonstate.LoginShellNodeProbe{
					Shell:      "/bin/zsh",
					Status:     daemonstate.LoginShellNodeStatusNotFound,
					Diagnostic: "no absolute node path in login shell output",
				},
			},
			want:       "node (login shell): not_found via /bin/zsh (no absolute node path in login shell output) — the daemon kept the service PATH; add node's directory to daemon_path_extra and run 'boss daemon restart'",
			wantRemedy: true,
		},
		{
			name: "timed_out",
			metadata: daemonstate.Metadata{
				LoginShellNode: &daemonstate.LoginShellNodeProbe{
					Shell:      "/bin/zsh",
					Status:     daemonstate.LoginShellNodeStatusTimedOut,
					Diagnostic: "login shell probe timed out after 10s: context deadline exceeded",
				},
			},
			want:       "node (login shell): timed_out via /bin/zsh (login shell probe timed out after 10s: context deadline exceeded) — the daemon kept the service PATH; add node's directory to daemon_path_extra and run 'boss daemon restart'",
			wantRemedy: true,
		},
		{
			name: "failed",
			metadata: daemonstate.Metadata{
				LoginShellNode: &daemonstate.LoginShellNodeProbe{
					Shell:      "/bin/bash",
					Status:     daemonstate.LoginShellNodeStatusFailed,
					Diagnostic: "exit status 1",
				},
			},
			want:       "node (login shell): failed via /bin/bash (exit status 1) — the daemon kept the service PATH; add node's directory to daemon_path_extra and run 'boss daemon restart'",
			wantRemedy: true,
		},
		{
			name: "failed without diagnostic or shell",
			metadata: daemonstate.Metadata{
				LoginShellNode: &daemonstate.LoginShellNodeProbe{
					Status: daemonstate.LoginShellNodeStatusFailed,
				},
			},
			want:       "node (login shell): failed via unknown shell — the daemon kept the service PATH; add node's directory to daemon_path_extra and run 'boss daemon restart'",
			wantRemedy: true,
		},
		{
			name: "skipped",
			metadata: daemonstate.Metadata{
				LoginShellNode: &daemonstate.LoginShellNodeProbe{
					Status: daemonstate.LoginShellNodeStatusSkipped,
				},
			},
			want: skippedLine,
		},
		{
			name:     "nil probe record",
			metadata: daemonstate.Metadata{},
			want:     notRecordedLine,
		},
		{
			name: "metadata read error overrides probe record",
			metadata: daemonstate.Metadata{
				LoginShellNode: &daemonstate.LoginShellNodeProbe{
					Shell:    "/bin/zsh",
					Status:   daemonstate.LoginShellNodeStatusPrepended,
					NodePath: "/opt/node/bin/node",
					Dir:      "/opt/node/bin",
				},
			},
			err:  errors.New("permission denied"),
			want: notRecordedLine,
		},
		{
			name: "unrecognized status falls back to not recorded",
			metadata: daemonstate.Metadata{
				LoginShellNode: &daemonstate.LoginShellNodeProbe{
					Status: daemonstate.LoginShellNodeStatus("future_status"),
				},
			},
			want: notRecordedLine,
		},
		{
			name: "sanitizes terminal control characters in probe fields",
			metadata: daemonstate.Metadata{
				LoginShellNode: &daemonstate.LoginShellNodeProbe{
					Shell:      "/bin/zsh\nFAKE",
					Status:     daemonstate.LoginShellNodeStatusFailed,
					Diagnostic: "exit\x1b[2Jstatus\u202e1",
				},
			},
			want:       "node (login shell): failed via /bin/zsh FAKE (exit [2Jstatus 1) — the daemon kept the service PATH; add node's directory to daemon_path_extra and run 'boss daemon restart'",
			wantRemedy: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			lines := daemonLoginShellNodeLines(tc.metadata, tc.err)
			if len(lines) != 1 {
				t.Fatalf("got %d lines (%q), want exactly 1", len(lines), lines)
			}
			got := lines[0]
			if got != tc.want {
				t.Errorf("line mismatch:\n got: %q\nwant: %q", got, tc.want)
			}
			hasRemedy := strings.Contains(got, remedyPhrase)
			if hasRemedy != tc.wantRemedy {
				t.Errorf("remedy presence = %v, want %v in %q", hasRemedy, tc.wantRemedy, got)
			}
			if !tc.wantRemedy && strings.Contains(got, "daemon_path_extra") {
				t.Errorf("unexpected daemon_path_extra remedy on non-failure status: %q", got)
			}
		})
	}
}

func setupHealthyDoctorWithMetadata(t *testing.T, writeMetadata func(appDataDir string)) *Group {
	t.Helper()
	deps := newTestGroup()
	deps.newDaemonDoctorAuthEnv(t)
	deps.stubRevisionDriftProbes(t, descendantRevisionDrift(), descendantRevisionDrift())
	deps.stubDaemonAuthStateProbe(t, &pb.GetAuthStateResponse{UpstreamConfigured: false}, nil)
	deps.credentialDeviceStatus = func(context.Context) (*pb.GetAgentCredentialDeviceStatusResponse, error) {
		return &pb.GetAgentCredentialDeviceStatusResponse{
			Readiness: &pb.CredentialDeviceReadiness{State: "unavailable"},
		}, nil
	}

	profile, err := deps.currentDaemonProfile()
	if err != nil {
		t.Fatalf("currentDaemonProfile: %v", err)
	}
	if writeMetadata != nil {
		writeMetadata(profile.AppDataDir)
	}
	return deps
}

func TestDaemonDoctorLoginShellNode_DaemonLifecycle_AC271101(t *testing.T) {
	deps := setupHealthyDoctorWithMetadata(t, func(appDataDir string) {
		if err := daemonstate.Write(appDataDir, daemonstate.Metadata{
			LoginShellNode: &daemonstate.LoginShellNodeProbe{
				Status: daemonstate.LoginShellNodeStatusSkipped,
			},
		}); err != nil {
			t.Fatalf("daemonstate.Write: %v", err)
		}
	})

	got, err := deps.runDaemonDoctorCapturing(t)
	if err != nil {
		t.Fatalf("runDaemonDoctor returned error %v, want healthy; output:\n%s", err, got)
	}
	if strings.Contains(got, "Remediation:") {
		t.Fatalf("unexpected Remediation block:\n%s", got)
	}

	const wantLine = "node (login shell): not probed — no supported login shell captured yet (run boss once, then 'boss daemon restart')"
	lines := strings.Split(strings.TrimRight(got, "\n"), "\n")

	serviceNodeIdx := -1
	serviceClaudeIdx := -1
	loginShellIdx := -1
	serviceSettingsIdx := -1
	loginShellCount := 0
	for i, line := range lines {
		switch {
		case strings.HasPrefix(line, "  node: "):
			serviceNodeIdx = i
		case strings.HasPrefix(line, "  claude: "):
			serviceClaudeIdx = i
		case strings.HasPrefix(line, "node (login shell): "):
			loginShellCount++
			loginShellIdx = i
			if line != wantLine {
				t.Errorf("login-shell line = %q, want %q", line, wantLine)
			}
		case strings.HasPrefix(line, "service settings: "):
			if serviceSettingsIdx == -1 {
				serviceSettingsIdx = i
			}
		}
	}

	if serviceNodeIdx < 0 {
		t.Fatalf("output missing service-PATH node line:\n%s", got)
	}
	if loginShellCount != 1 {
		t.Fatalf("found %d login-shell node lines, want exactly 1:\n%s", loginShellCount, got)
	}
	if serviceClaudeIdx < 0 || loginShellIdx != serviceClaudeIdx+1 {
		t.Fatalf("login-shell line (index %d) does not directly follow service-PATH lines (claude index %d):\n%s", loginShellIdx, serviceClaudeIdx, got)
	}
	if serviceSettingsIdx < 0 || loginShellIdx >= serviceSettingsIdx {
		t.Fatalf("login-shell line (index %d) must precede service settings (index %d):\n%s", loginShellIdx, serviceSettingsIdx, got)
	}
}

func TestDaemonLoginShellNodeLines_DaemonLifecycle_AC271102(t *testing.T) {
	cases := []struct {
		name          string
		writeMetadata func(t *testing.T, appDataDir string)
		wantLine      string
	}{
		{
			name: "prepended",
			writeMetadata: func(t *testing.T, appDataDir string) {
				if err := daemonstate.Write(appDataDir, daemonstate.Metadata{
					LoginShellNode: &daemonstate.LoginShellNodeProbe{
						Shell:    "/bin/zsh",
						Status:   daemonstate.LoginShellNodeStatusPrepended,
						NodePath: "/Users/dev/.nvm/versions/node/v22.12.0/bin/node",
						Dir:      "/Users/dev/.nvm/versions/node/v22.12.0/bin",
					},
				}); err != nil {
					t.Fatalf("daemonstate.Write: %v", err)
				}
			},
			wantLine: "node (login shell): /Users/dev/.nvm/versions/node/v22.12.0/bin/node — bossd prepended /Users/dev/.nvm/versions/node/v22.12.0/bin to its PATH",
		},
		{
			name: "on_path",
			writeMetadata: func(t *testing.T, appDataDir string) {
				if err := daemonstate.Write(appDataDir, daemonstate.Metadata{
					LoginShellNode: &daemonstate.LoginShellNodeProbe{
						Shell:    "/bin/zsh",
						Status:   daemonstate.LoginShellNodeStatusOnPath,
						NodePath: "/opt/homebrew/bin/node",
						Dir:      "/opt/homebrew/bin",
					},
				}); err != nil {
					t.Fatalf("daemonstate.Write: %v", err)
				}
			},
			wantLine: "node (login shell): /opt/homebrew/bin/node — already on the daemon's PATH",
		},
		{
			name: "not_found",
			writeMetadata: func(t *testing.T, appDataDir string) {
				if err := daemonstate.Write(appDataDir, daemonstate.Metadata{
					LoginShellNode: &daemonstate.LoginShellNodeProbe{
						Shell:      "/bin/zsh",
						Status:     daemonstate.LoginShellNodeStatusNotFound,
						Diagnostic: "no absolute node path in login shell output",
					},
				}); err != nil {
					t.Fatalf("daemonstate.Write: %v", err)
				}
			},
			wantLine: "node (login shell): not_found via /bin/zsh (no absolute node path in login shell output) — the daemon kept the service PATH; add node's directory to daemon_path_extra and run 'boss daemon restart'",
		},
		{
			name: "timed_out",
			writeMetadata: func(t *testing.T, appDataDir string) {
				if err := daemonstate.Write(appDataDir, daemonstate.Metadata{
					LoginShellNode: &daemonstate.LoginShellNodeProbe{
						Shell:      "/bin/zsh",
						Status:     daemonstate.LoginShellNodeStatusTimedOut,
						Diagnostic: "login shell probe timed out after 10s",
					},
				}); err != nil {
					t.Fatalf("daemonstate.Write: %v", err)
				}
			},
			wantLine: "node (login shell): timed_out via /bin/zsh (login shell probe timed out after 10s) — the daemon kept the service PATH; add node's directory to daemon_path_extra and run 'boss daemon restart'",
		},
		{
			name: "failed",
			writeMetadata: func(t *testing.T, appDataDir string) {
				if err := daemonstate.Write(appDataDir, daemonstate.Metadata{
					LoginShellNode: &daemonstate.LoginShellNodeProbe{
						Shell:      "/bin/bash",
						Status:     daemonstate.LoginShellNodeStatusFailed,
						Diagnostic: "exit status 127",
					},
				}); err != nil {
					t.Fatalf("daemonstate.Write: %v", err)
				}
			},
			wantLine: "node (login shell): failed via /bin/bash (exit status 127) — the daemon kept the service PATH; add node's directory to daemon_path_extra and run 'boss daemon restart'",
		},
		{
			name: "legacy nil record",
			writeMetadata: func(t *testing.T, appDataDir string) {
				if err := daemonstate.Write(appDataDir, daemonstate.Metadata{}); err != nil {
					t.Fatalf("daemonstate.Write: %v", err)
				}
			},
			wantLine: "node (login shell): not recorded (the running daemon predates this check, or its state file could not be read)",
		},
		{
			name:          "missing metadata file",
			writeMetadata: nil,
			wantLine:      "node (login shell): not recorded (the running daemon predates this check, or its state file could not be read)",
		},
		{
			name: "unreadable corrupt metadata file",
			writeMetadata: func(t *testing.T, appDataDir string) {
				if err := os.MkdirAll(appDataDir, 0o700); err != nil {
					t.Fatalf("mkdir appDataDir: %v", err)
				}
				if err := os.WriteFile(daemonstate.Path(appDataDir), []byte("{corrupt"), 0o600); err != nil {
					t.Fatalf("write corrupt metadata: %v", err)
				}
			},
			wantLine: "node (login shell): not recorded (the running daemon predates this check, or its state file could not be read)",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var writer func(string)
			if tc.writeMetadata != nil {
				writer = func(appDataDir string) { tc.writeMetadata(t, appDataDir) }
			}
			deps := setupHealthyDoctorWithMetadata(t, writer)

			got, err := deps.runDaemonDoctorCapturing(t)
			if err != nil {
				t.Fatalf("runDaemonDoctor returned error %v, want nil (advisory only); output:\n%s", err, got)
			}
			if strings.Contains(got, "Remediation:") {
				t.Fatalf("advisory login-shell status added a Remediation block:\n%s", got)
			}
			if !strings.Contains(got, tc.wantLine+"\n") {
				t.Fatalf("output missing expected line %q:\n%s", tc.wantLine, got)
			}
		})
	}
}
