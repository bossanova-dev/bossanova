package accountflow

import (
	"errors"
	"os/exec"
	"reflect"
	"strings"
	"testing"
)

func TestResolveAgentBin_Accounts_AC224001_AC224002_AC224003(t *testing.T) {
	for _, tc := range []struct {
		name, explicit, callerHit, serviceHit, service, flag, wantPath, wantEnv string
		wantError                                                               []string
	}{
		{name: "explicit", explicit: "/chosen/codex", wantPath: "/chosen/codex"},
		{name: "explicit miss", explicit: "/missing", wantError: []string{"/missing", "--codex-bin"}},
		{name: "caller", callerHit: "/caller/codex", wantPath: "/caller/codex"},
		{name: "service", service: "/service:/shared", serviceHit: "/service/codex", wantPath: "/service/codex", wantEnv: "PATH=/caller:/shared:/service"},
		{name: "missing", service: "/service", wantError: []string{"codex", "/caller:/shared", "/service", "--codex-bin /absolute/path/to/codex", `env PATH="/absolute/path/to:$PATH" boss account reauth account-1`, "daemon_path_extra", "boss daemon restart"}},
		{name: "unknown", wantError: []string{"unknown"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			lookups := 0
			serviceLookups := 0
			got, err := ResolveAgentBin(AgentBinSearch{Name: "codex", Explicit: tc.explicit, ExplicitSet: tc.explicit != "", ExplicitFlag: "--codex-bin", CallerPATH: "/caller:/shared", ServicePATH: tc.service, Rerun: "boss account reauth account-1", LookPath: func(s string) (string, error) {
				lookups++
				if tc.explicit != "" && tc.name == "explicit" {
					return s, nil
				}
				if tc.callerHit != "" {
					return tc.callerHit, nil
				}
				return "", errors.New("missing")
			}, LookPathIn: func(path, name string) (string, bool) { serviceLookups++; return tc.serviceHit, tc.serviceHit != "" }})
			if len(tc.wantError) > 0 {
				if err == nil {
					t.Fatal("expected error")
				}
				for _, s := range tc.wantError {
					if !strings.Contains(err.Error(), s) {
						t.Errorf("%q missing %q", err, s)
					}
				}
			} else {
				if err != nil {
					t.Fatal(err)
				}
				if got.Path != tc.wantPath {
					t.Errorf("path=%q", got.Path)
				}
				var env []string
				if tc.wantEnv != "" {
					env = []string{tc.wantEnv}
				}
				if !reflect.DeepEqual(got.Env, env) {
					t.Errorf("env=%v want %v", got.Env, env)
				}
			}
			if tc.explicit != "" && (lookups != 1 || serviceLookups != 0) {
				t.Errorf("explicit lookup fell back: %d/%d", lookups, serviceLookups)
			}
		})
	}
}

func TestResolveAgentBinClaudeRemedy(t *testing.T) {
	_, err := ResolveAgentBin(AgentBinSearch{Name: "claude", CallerPATH: "/shell", Rerun: "boss account add claude", LookPath: func(string) (string, error) { return "", errors.New("missing") }})
	if err == nil || strings.Contains(err.Error(), "--codex-bin") || !strings.Contains(err.Error(), "boss account add claude") {
		t.Fatalf("err=%v", err)
	}
}

func TestResolveAgentBinExplicitEmpty(t *testing.T) {
	_, err := ResolveAgentBin(AgentBinSearch{Name: "codex", ExplicitSet: true, ExplicitFlag: "--codex-bin", LookPath: func(string) (string, error) { t.Fatal("empty override must not search PATH"); return "", nil }, LookPathIn: func(string, string) (string, bool) {
		t.Fatal("empty override must not search service PATH")
		return "", false
	}})
	if err == nil || !strings.Contains(err.Error(), `--codex-bin ""`) {
		t.Fatalf("err=%v", err)
	}
}

func TestResolveAgentBinRejectsRelativePathExecutables(t *testing.T) {
	for _, tc := range []struct {
		name       string
		callerErr  error
		serviceBin string
	}{
		{"caller ErrDot", exec.ErrDot, "/service/codex"},
		{"relative service", exec.ErrNotFound, "bin/codex"},
		{"dot service", exec.ErrNotFound, "codex"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ResolveAgentBin(AgentBinSearch{Name: "codex", CallerPATH: "bin", ServicePATH: "bin:/service", LookPath: func(string) (string, error) { return "bin/codex", tc.callerErr }, LookPathIn: func(string, string) (string, bool) { return tc.serviceBin, true }})
			if !errors.Is(err, exec.ErrDot) || got.Path != "" {
				t.Fatalf("got=%+v err=%v; want ErrDot", got, err)
			}
		})
	}
}
