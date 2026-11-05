package accountflow

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// AgentBinSearch supplies the provider CLI search paths and executable lookups.
// LookPath searches the caller's environment; LookPathIn searches a supplied PATH.
type AgentBinSearch struct {
	Name, Explicit, ExplicitFlag   string
	ExplicitSet                    bool // override supplied, including an explicitly empty value
	CallerPATH, ServicePATH, Rerun string
	LookPath                       func(string) (string, error)
	LookPathIn                     func(pathValue, name string) (string, bool)
}

// AgentBin names an executable and any environment needed by its launcher.
type AgentBin struct {
	Path string
	Env  []string
}

// ResolveAgentBin prefers an explicit choice, then the caller's PATH, then the
// daemon's service PATH. An invalid explicit choice never falls back.
func ResolveAgentBin(s AgentBinSearch) (AgentBin, error) {
	if s.ExplicitSet {
		if s.Explicit == "" {
			return AgentBin{}, fmt.Errorf("resolve %s %q: executable must not be empty", s.ExplicitFlag, s.Explicit)
		}
		path, err := s.LookPath(s.Explicit)
		if err != nil {
			return AgentBin{}, fmt.Errorf("resolve %s %q: %w", s.ExplicitFlag, s.Explicit, err)
		}
		return AgentBin{Path: path}, nil
	}
	if path, err := s.LookPath(s.Name); err == nil {
		return AgentBin{Path: path}, nil
	} else if errors.Is(err, exec.ErrDot) {
		return AgentBin{}, fmt.Errorf("resolve %s: %w", s.Name, err)
	}
	if s.ServicePATH != "" && s.LookPathIn != nil {
		if path, ok := s.LookPathIn(s.ServicePATH, s.Name); ok {
			if !filepath.IsAbs(path) {
				return AgentBin{}, fmt.Errorf("resolve %s from daemon service PATH: %w", s.Name, exec.ErrDot)
			}
			return AgentBin{Path: path, Env: []string{"PATH=" + agentBinEnvPath(s.CallerPATH, s.ServicePATH)}}, nil
		}
	}
	service := fmt.Sprintf("%q", s.ServicePATH)
	if s.ServicePATH == "" {
		service = "unknown"
	}
	remedy := ""
	if s.ExplicitFlag != "" {
		remedy = fmt.Sprintf("\n  %s %s /absolute/path/to/%s", s.Rerun, s.ExplicitFlag, s.Name)
	}
	return AgentBin{}, fmt.Errorf("cannot find %s; searched shell PATH %q and daemon service PATH %s. Remedies:%s\n  env PATH=\"/absolute/path/to:$PATH\" %s\n  Add the directory containing %s to daemon_path_extra in Boss settings, then run `boss daemon restart`", s.Name, s.CallerPATH, service, remedy, s.Rerun, s.Name)
}

func agentBinEnvPath(caller, service string) string {
	seen := map[string]bool{}
	var entries []string
	for _, path := range []string{caller, service} {
		if path == "" {
			continue
		}
		for _, entry := range strings.Split(path, string(os.PathListSeparator)) {
			if !seen[entry] {
				seen[entry] = true
				entries = append(entries, entry)
			}
		}
	}
	return strings.Join(entries, string(os.PathListSeparator))
}
