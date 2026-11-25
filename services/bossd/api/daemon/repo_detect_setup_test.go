package server

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"connectrpc.com/connect"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/rs/zerolog"
	"google.golang.org/protobuf/proto"
)

func TestDetectSetupScript(t *testing.T) {
	dir := t.TempDir()
	filePath := filepath.Join(dir, "not-a-dir")
	if err := os.WriteFile(filePath, []byte("x"), 0o600); err != nil {
		t.Fatalf("write file: %v", err)
	}

	const detectedJSON = `{"type":"command","argv":["pnpm","install","--frozen-lockfile"]}`

	t.Run("returns detected spec and evidence for non-git fixture", func(t *testing.T) {
		var gotPath string
		wantEvidence := []string{".devcontainer/devcontainer.json declares postCreateCommand (not adopted)"}
		s := New(Config{
			DetectSetupScript: func(_ context.Context, path string) (SetupScriptDetection, error) {
				gotPath = path
				return SetupScriptDetection{
					SetupScript:    detectedJSON,
					DisplayCommand: "pnpm install --frozen-lockfile",
					Source:         "pnpm-lock.yaml",
					Evidence:       wantEvidence,
				}, nil
			},
		})

		resp, err := s.DetectSetupScript(context.Background(), connect.NewRequest(&pb.DetectSetupScriptRequest{
			LocalPath: dir,
		}))
		if err != nil {
			t.Fatalf("DetectSetupScript() error = %v", err)
		}
		if gotPath != dir {
			t.Fatalf("detector path = %q, want %q", gotPath, dir)
		}
		if resp.Msg.GetSetupScript() != detectedJSON {
			t.Errorf("SetupScript = %q, want %q", resp.Msg.GetSetupScript(), detectedJSON)
		}
		if resp.Msg.GetDisplayCommand() != "pnpm install --frozen-lockfile" {
			t.Errorf("DisplayCommand = %q, want %q", resp.Msg.GetDisplayCommand(), "pnpm install --frozen-lockfile")
		}
		if resp.Msg.GetSource() != "pnpm-lock.yaml" {
			t.Errorf("Source = %q, want %q", resp.Msg.GetSource(), "pnpm-lock.yaml")
		}
		if !reflect.DeepEqual(resp.Msg.GetEvidence(), wantEvidence) {
			t.Errorf("Evidence = %v, want %v", resp.Msg.GetEvidence(), wantEvidence)
		}
	})

	t.Run("empty detection returns empty response with evidence", func(t *testing.T) {
		wantEvidence := []string{"bin/setup exists but is not executable"}
		s := New(Config{
			DetectSetupScript: func(context.Context, string) (SetupScriptDetection, error) {
				return SetupScriptDetection{Evidence: wantEvidence}, nil
			},
		})

		resp, err := s.DetectSetupScript(context.Background(), connect.NewRequest(&pb.DetectSetupScriptRequest{
			LocalPath: dir,
		}))
		if err != nil {
			t.Fatalf("DetectSetupScript() error = %v", err)
		}
		if resp.Msg.GetSetupScript() != "" || resp.Msg.GetDisplayCommand() != "" || resp.Msg.GetSource() != "" {
			t.Fatalf("got non-empty detection fields: %+v", resp.Msg)
		}
		if !reflect.DeepEqual(resp.Msg.GetEvidence(), wantEvidence) {
			t.Errorf("Evidence = %v, want %v", resp.Msg.GetEvidence(), wantEvidence)
		}
	})

	for _, tc := range []struct {
		name      string
		localPath string
	}{
		{name: "empty path", localPath: ""},
		{name: "missing path", localPath: filepath.Join(dir, "missing")},
		{name: "file path", localPath: filePath},
	} {
		t.Run(tc.name+" returns InvalidArgument", func(t *testing.T) {
			called := false
			s := New(Config{
				DetectSetupScript: func(context.Context, string) (SetupScriptDetection, error) {
					called = true
					return SetupScriptDetection{}, nil
				},
			})

			_, err := s.DetectSetupScript(context.Background(), connect.NewRequest(&pb.DetectSetupScriptRequest{
				LocalPath: tc.localPath,
			}))
			if got := connect.CodeOf(err); got != connect.CodeInvalidArgument {
				t.Fatalf("DetectSetupScript() code = %v, want InvalidArgument (err=%v)", got, err)
			}
			if called {
				t.Fatal("detector was called for invalid path")
			}
		})
	}

	t.Run("nil detector returns Unimplemented", func(t *testing.T) {
		s := New(Config{})
		_, err := s.DetectSetupScript(context.Background(), connect.NewRequest(&pb.DetectSetupScriptRequest{
			LocalPath: dir,
		}))
		if got := connect.CodeOf(err); got != connect.CodeUnimplemented {
			t.Fatalf("DetectSetupScript() code = %v, want Unimplemented (err=%v)", got, err)
		}
	})

	t.Run("detector error returns Internal", func(t *testing.T) {
		s := New(Config{
			DetectSetupScript: func(context.Context, string) (SetupScriptDetection, error) {
				return SetupScriptDetection{}, errors.New("scan failed")
			},
		})
		_, err := s.DetectSetupScript(context.Background(), connect.NewRequest(&pb.DetectSetupScriptRequest{
			LocalPath: dir,
		}))
		if got := connect.CodeOf(err); got != connect.CodeInternal {
			t.Fatalf("DetectSetupScript() code = %v, want Internal (err=%v)", got, err)
		}
	})
}

func TestRegisterRepo_DetectSetupScript(t *testing.T) {
	seedWorktreeBaseSettings(t)
	dir := t.TempDir()

	const detectedJSON = `{"type":"make","target":"setup-worktree"}`
	explicit := "make custom"
	empty := ""

	tests := []struct {
		name           string
		req            *pb.RegisterRepoRequest
		nilDetector    bool
		detectResult   SetupScriptDetection
		detectErr      error
		wantDetectCall int
		wantSetup      *string
		wantWarnLog    bool
	}{
		{
			name:           "flag true with absent setup_script adopts detected spec",
			req:            &pb.RegisterRepoRequest{LocalPath: dir, ShouldDetectSetupScript: proto.Bool(true)},
			detectResult:   SetupScriptDetection{SetupScript: detectedJSON, DisplayCommand: "make setup-worktree", Source: "Makefile target setup-worktree"},
			wantDetectCall: 1,
			wantSetup:      proto.String(detectedJSON),
		},
		{
			name:           "flag true with no detection stores nil",
			req:            &pb.RegisterRepoRequest{LocalPath: dir, ShouldDetectSetupScript: proto.Bool(true)},
			detectResult:   SetupScriptDetection{Evidence: []string{"bin/setup exists but is not executable"}},
			wantDetectCall: 1,
			wantSetup:      nil,
		},
		{
			name:           "flag false stores nil and skips detector",
			req:            &pb.RegisterRepoRequest{LocalPath: dir, ShouldDetectSetupScript: proto.Bool(false)},
			detectResult:   SetupScriptDetection{SetupScript: detectedJSON},
			wantDetectCall: 0,
			wantSetup:      nil,
		},
		{
			name:           "flag absent stores nil and skips detector",
			req:            &pb.RegisterRepoRequest{LocalPath: dir},
			detectResult:   SetupScriptDetection{SetupScript: detectedJSON},
			wantDetectCall: 0,
			wantSetup:      nil,
		},
		{
			name:           "explicit setup_script wins over flag true",
			req:            &pb.RegisterRepoRequest{LocalPath: dir, SetupScript: &explicit, ShouldDetectSetupScript: proto.Bool(true)},
			detectResult:   SetupScriptDetection{SetupScript: detectedJSON},
			wantDetectCall: 0,
			wantSetup:      &explicit,
		},
		{
			name:           "explicit empty setup_script wins over flag true",
			req:            &pb.RegisterRepoRequest{LocalPath: dir, SetupScript: &empty, ShouldDetectSetupScript: proto.Bool(true)},
			detectResult:   SetupScriptDetection{SetupScript: detectedJSON},
			wantDetectCall: 0,
			wantSetup:      &empty,
		},
		{
			name:           "detector error logs warn and registers with nil setup_script",
			req:            &pb.RegisterRepoRequest{LocalPath: dir, ShouldDetectSetupScript: proto.Bool(true)},
			detectErr:      errors.New("permission denied"),
			wantDetectCall: 1,
			wantSetup:      nil,
			wantWarnLog:    true,
		},
		{
			name:           "nil detector registers with nil setup_script",
			req:            &pb.RegisterRepoRequest{LocalPath: dir, ShouldDetectSetupScript: proto.Bool(true)},
			nilDetector:    true,
			wantDetectCall: 0,
			wantSetup:      nil,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			repos := &repoMgmtRepoStore{}
			wt := &repoMgmtWorktree{isGitRepo: true, originURL: "https://github.com/acme/repo.git"}
			var logBuf bytes.Buffer
			var detectCalls int
			var detectPath string

			cfg := Config{
				Repos:     repos,
				Worktrees: wt,
				Logger:    zerolog.New(&logBuf),
			}
			if !tc.nilDetector {
				cfg.DetectSetupScript = func(_ context.Context, path string) (SetupScriptDetection, error) {
					detectCalls++
					detectPath = path
					return tc.detectResult, tc.detectErr
				}
			}

			s := New(cfg)
			resp, err := s.RegisterRepo(context.Background(), connect.NewRequest(tc.req))
			if err != nil {
				t.Fatalf("RegisterRepo() error = %v", err)
			}
			if resp.Msg.GetRepo() == nil {
				t.Fatal("RegisterRepo() returned nil repo")
			}
			if detectCalls != tc.wantDetectCall {
				t.Fatalf("detector calls = %d, want %d", detectCalls, tc.wantDetectCall)
			}
			if detectCalls > 0 && detectPath != dir {
				t.Fatalf("detector path = %q, want %q", detectPath, dir)
			}
			assertOptionalString(t, "Create SetupScript", repos.createParams.SetupScript, tc.wantSetup)
			if tc.wantWarnLog {
				logged := logBuf.String()
				if !strings.Contains(logged, `"level":"warn"`) || !strings.Contains(logged, dir) {
					t.Fatalf("warn log = %q, want level=warn and path %q", logged, dir)
				}
			}
		})
	}
}

func TestCloneAndRegisterRepo_DetectSetupScript(t *testing.T) {
	seedWorktreeBaseSettings(t)

	const detectedJSON = `{"type":"command","argv":["pnpm","install","--frozen-lockfile"]}`
	explicit := "make custom"
	empty := ""

	tests := []struct {
		name           string
		setupScript    *string
		shouldDetect   *bool
		nilDetector    bool
		detectResult   SetupScriptDetection
		detectErr      error
		wantDetectCall int
		wantSetup      *string
		wantWarnLog    bool
	}{
		{
			name:           "flag true with absent setup_script adopts detected spec after clone",
			shouldDetect:   proto.Bool(true),
			detectResult:   SetupScriptDetection{SetupScript: detectedJSON, DisplayCommand: "pnpm install --frozen-lockfile", Source: "pnpm-lock.yaml"},
			wantDetectCall: 1,
			wantSetup:      proto.String(detectedJSON),
		},
		{
			name:           "flag true with no detection stores nil",
			shouldDetect:   proto.Bool(true),
			detectResult:   SetupScriptDetection{},
			wantDetectCall: 1,
			wantSetup:      nil,
		},
		{
			name:           "flag false stores nil and skips detector",
			shouldDetect:   proto.Bool(false),
			detectResult:   SetupScriptDetection{SetupScript: detectedJSON},
			wantDetectCall: 0,
			wantSetup:      nil,
		},
		{
			name:           "flag absent stores nil and skips detector",
			detectResult:   SetupScriptDetection{SetupScript: detectedJSON},
			wantDetectCall: 0,
			wantSetup:      nil,
		},
		{
			name:           "explicit setup_script wins over flag true",
			setupScript:    &explicit,
			shouldDetect:   proto.Bool(true),
			detectResult:   SetupScriptDetection{SetupScript: detectedJSON},
			wantDetectCall: 0,
			wantSetup:      &explicit,
		},
		{
			name:           "explicit empty setup_script wins over flag true",
			setupScript:    &empty,
			shouldDetect:   proto.Bool(true),
			detectResult:   SetupScriptDetection{SetupScript: detectedJSON},
			wantDetectCall: 0,
			wantSetup:      &empty,
		},
		{
			name:           "detector error logs warn and registers with nil setup_script",
			shouldDetect:   proto.Bool(true),
			detectErr:      errors.New("unreadable"),
			wantDetectCall: 1,
			wantSetup:      nil,
			wantWarnLog:    true,
		},
		{
			name:           "nil detector registers with nil setup_script",
			shouldDetect:   proto.Bool(true),
			nilDetector:    true,
			wantDetectCall: 0,
			wantSetup:      nil,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cloneDir := filepath.Join(t.TempDir(), "cloned")
			repos := &repoMgmtRepoStore{}
			wt := &repoMgmtWorktree{originURL: "git@github.com:acme/repo.git"}
			var logBuf bytes.Buffer
			var detectCalls int
			var detectPath string
			var clonesBeforeDetect int

			cfg := Config{
				Repos:     repos,
				Worktrees: wt,
				Logger:    zerolog.New(&logBuf),
			}
			if !tc.nilDetector {
				cfg.DetectSetupScript = func(_ context.Context, path string) (SetupScriptDetection, error) {
					detectCalls++
					detectPath = path
					clonesBeforeDetect = wt.cloneCalls
					return tc.detectResult, tc.detectErr
				}
			}

			s := New(cfg)
			resp, err := s.CloneAndRegisterRepo(context.Background(), connect.NewRequest(&pb.CloneAndRegisterRepoRequest{
				CloneUrl:                "git@github.com:acme/repo.git",
				LocalPath:               cloneDir,
				SetupScript:             tc.setupScript,
				ShouldDetectSetupScript: tc.shouldDetect,
			}))
			if err != nil {
				t.Fatalf("CloneAndRegisterRepo() error = %v", err)
			}
			if resp.Msg.GetRepo() == nil {
				t.Fatal("CloneAndRegisterRepo() returned nil repo")
			}
			if wt.cloneCalls != 1 {
				t.Fatalf("cloneCalls = %d, want 1", wt.cloneCalls)
			}
			if detectCalls != tc.wantDetectCall {
				t.Fatalf("detector calls = %d, want %d", detectCalls, tc.wantDetectCall)
			}
			if detectCalls > 0 {
				if clonesBeforeDetect != 1 {
					t.Fatalf("detector ran before clone (cloneCalls=%d)", clonesBeforeDetect)
				}
				if detectPath != cloneDir {
					t.Fatalf("detector path = %q, want %q", detectPath, cloneDir)
				}
			}
			assertOptionalString(t, "Create SetupScript", repos.createParams.SetupScript, tc.wantSetup)
			if tc.wantWarnLog {
				logged := logBuf.String()
				if !strings.Contains(logged, `"level":"warn"`) || !strings.Contains(logged, cloneDir) {
					t.Fatalf("warn log = %q, want level=warn and path %q", logged, cloneDir)
				}
			}
		})
	}
}

func assertOptionalString(t *testing.T, label string, got, want *string) {
	t.Helper()
	if (got == nil) != (want == nil) {
		t.Fatalf("%s = %v, want nil=%v", label, got, want == nil)
	}
	if want != nil && *got != *want {
		t.Fatalf("%s = %q, want %q", label, *got, *want)
	}
}
