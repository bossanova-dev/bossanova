package repo

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/recurser/boss/api/cli/skills"
	daemonrpc "github.com/recurser/boss/internal/adapter/daemonrpc"
	"github.com/recurser/boss/internal/auth"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/spf13/cobra"
)

func initGitRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	for _, args := range [][]string{{"init", "-b", "main", dir}, {"-C", dir, "-c", "user.name=Test", "-c", "user.email=test@example.com", "commit", "--allow-empty", "-m", "initial"}} {
		if out, err := exec.Command("git", args...).CombinedOutput(); err != nil {
			t.Fatalf("git: %v: %s", err, out)
		}
	}
	return dir
}

type cronFixture struct {
	home string
	env  skills.CronEnv
}

func newCronFixture(t *testing.T, _ ...string) *cronFixture {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("shell-script stub executables are POSIX-only")
	}
	home, service := t.TempDir(), t.TempDir()
	for _, agent := range []string{".claude", ".codex"} {
		for _, stage := range []string{"plan", "build", "verify", "retro"} {
			for _, rel := range []string{"boss-" + stage + "/toolbox/cron-gates/boss-" + stage + ".mjs", "boss-" + stage + "/toolbox/skill-config.mjs"} {
				p := filepath.Join(home, agent, "skills", filepath.FromSlash(rel))
				if err := os.MkdirAll(filepath.Dir(p), 0755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(p, []byte("// stub\n"), 0644); err != nil {
					t.Fatal(err)
				}
			}
		}
	}
	for _, tool := range []string{"node", "claude", "codex"} {
		if err := os.WriteFile(filepath.Join(service, tool), []byte("#!/bin/sh\nexit 0\n"), 0755); err != nil {
			t.Fatal(err)
		}
	}
	str := func(s string) *string { return &s }
	entry := func(state string) skills.CronEffectiveEntry {
		return skills.CronEffectiveEntry{Value: &skills.CronStageQuery{State: str(state), Selection: map[string]map[string][]string{}}}
	}
	return &cronFixture{home: home, env: skills.CronEnv{
		ServicePath:    func() string { return service },
		CallerLookPath: func(string) (string, error) { return "", errors.New("not found") },
		HomeDir:        func() (string, error) { return home, nil },
		SkillRoot:      func(agent string) (string, error) { return filepath.Join(home, "."+agent, "skills"), nil },
		DefaultBranch:  func(context.Context, string, *pb.Repo) string { return "main" },
		ReadRef: func(_ context.Context, _, ref, p string) ([]byte, bool, error) {
			if ref != "origin/main" {
				return nil, false, errors.New("unexpected ref " + ref)
			}
			switch p {
			case ".mcp.json":
				return []byte(`{"mcpServers":{"linear":{"type":"http"}}}`), true, nil
			case ".codex/config.toml":
				return []byte("[mcp_servers.\"linear\"]\nurl = \"x\"\n"), true, nil
			}
			return nil, false, nil
		},
		Validate: func(string, string, []byte, string) error { return nil },
		Effective: func(string, string, []byte) (skills.CronEffective, error) {
			return skills.CronEffective{Plan: entry("Backlog"), Build: entry("Todo"), Verify: entry("In Review"), MCPServer: skills.CronEffectiveString{Value: str("linear")}}, nil
		},
	}}
}

func retroGate(agentDir string) string {
	return `node "$HOME/` + agentDir + `/skills/boss-retro/toolbox/cron-gates/boss-retro.mjs"`
}

func deref(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

type emptyTokenStore struct{}

func (emptyTokenStore) Save(*auth.Tokens) error     { return nil }
func (emptyTokenStore) Load() (*auth.Tokens, error) { return nil, errors.New("no stored tokens") }
func (emptyTokenStore) Delete() error               { return nil }

func newTestGroup() *Group { return New(testClient, Options{}) }
func testClient(*cobra.Command) (Client, error) {
	socket, err := daemonrpc.DefaultSocketPath()
	if err != nil {
		return nil, err
	}
	return daemonrpc.NewLocal(socket), nil
}
