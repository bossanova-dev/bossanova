package server

import (
	"errors"
	"strings"
	"testing"

	"connectrpc.com/connect"

	"github.com/recurser/bossalib/config"
)

func TestResolveRepoWorktreeBaseDir(t *testing.T) {
	t.Parallel()

	loaderReturning := func(dir string) func() (config.Settings, error) {
		return func() (config.Settings, error) {
			return config.Settings{WorktreeBaseDir: dir}, nil
		}
	}
	failingLoader := func() (config.Settings, error) {
		return config.Settings{WorktreeBaseDir: "/should/not/be/used"}, errors.New("settings unreadable")
	}
	unusedLoader := func() (config.Settings, error) {
		t.Error("loader called for an explicit worktree_base_dir")
		return config.Settings{}, nil
	}

	tests := []struct {
		name       string
		requested  string
		load       func() (config.Settings, error)
		want       string
		wantCode   connect.Code
		wantErrSub string
	}{
		{name: "explicit absolute is kept and cleaned", requested: " /srv/wt/../worktrees/ ", load: unusedLoader, want: "/srv/worktrees"},
		{name: "explicit relative is rejected", requested: "worktrees", load: unusedLoader, wantCode: connect.CodeInvalidArgument, wantErrSub: "absolute"},
		{name: "explicit tilde is rejected", requested: "~/x", load: unusedLoader, wantCode: connect.CodeInvalidArgument, wantErrSub: "absolute"},
		{name: "whitespace falls back to loaded default", requested: "  \t", load: loaderReturning("/home/u/.bossanova/worktrees"), want: "/home/u/.bossanova/worktrees"},
		{name: "empty uses stub loader value", requested: "", load: loaderReturning("/data/wt"), want: "/data/wt"},
		{name: "loader error is internal", requested: "", load: failingLoader, wantCode: connect.CodeInternal, wantErrSub: "load settings"},
		{name: "blank global default is failed precondition", requested: "", load: loaderReturning("  "), wantCode: connect.CodeFailedPrecondition, wantErrSub: "worktree_base_dir"},
		{name: "relative global default is failed precondition", requested: "", load: loaderReturning("rel/dir"), wantCode: connect.CodeFailedPrecondition, wantErrSub: "global settings.worktree_base_dir"},
		{name: "tilde global default is failed precondition", requested: "", load: loaderReturning("~/wt"), wantCode: connect.CodeFailedPrecondition, wantErrSub: "not absolute"},
		{name: "global default is cleaned", requested: "", load: loaderReturning(" /data/wt/ "), want: "/data/wt"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := resolveRepoWorktreeBaseDir(tc.requested, tc.load)
			if tc.wantCode != 0 {
				if err == nil {
					t.Fatalf("resolveRepoWorktreeBaseDir() = %q, want error code %v", got, tc.wantCode)
				}
				if code := connect.CodeOf(err); code != tc.wantCode {
					t.Fatalf("code = %v, want %v (err=%v)", code, tc.wantCode, err)
				}
				if !strings.Contains(err.Error(), tc.wantErrSub) {
					t.Fatalf("error = %v, want substring %q", err, tc.wantErrSub)
				}
				if got != "" {
					t.Fatalf("got %q alongside an error, want empty", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("resolveRepoWorktreeBaseDir() error = %v", err)
			}
			if got != tc.want {
				t.Fatalf("resolveRepoWorktreeBaseDir() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestValidateRepoWorktreeBaseDir(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		in      string
		want    string
		wantErr bool
	}{
		{name: "absolute cleaned", in: "/a/b/", want: "/a/b"},
		{name: "empty rejected", in: "", wantErr: true},
		{name: "whitespace rejected", in: "   ", wantErr: true},
		{name: "relative rejected", in: "a/b", wantErr: true},
		{name: "tilde rejected", in: "~/wt", wantErr: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := validateRepoWorktreeBaseDir(tc.in)
			if tc.wantErr {
				if connect.CodeOf(err) != connect.CodeInvalidArgument {
					t.Fatalf("validateRepoWorktreeBaseDir(%q) err = %v, want InvalidArgument", tc.in, err)
				}
				return
			}
			if err != nil || got != tc.want {
				t.Fatalf("validateRepoWorktreeBaseDir(%q) = %q, %v; want %q", tc.in, got, err, tc.want)
			}
		})
	}
}
