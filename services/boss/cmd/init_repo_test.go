package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/recurser/boss/internal/client"
	"github.com/recurser/boss/internal/tuitest"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/spf13/cobra"
)

func initGitRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	for _, args := range [][]string{{"init", "-b", "main", dir}, {"-C", dir, "-c", "user.name=Test", "-c", "user.email=test@example.com", "commit", "--allow-empty", "-m", "initial"}} {
		if output, err := exec.Command("git", args...).CombinedOutput(); err != nil {
			t.Fatalf("git: %v: %s", err, output)
		}
	}
	return dir
}

func TestInitRepoRegistration(t *testing.T) {
	for _, mode := range []string{"register", "symlink", "worktree"} {
		t.Run(mode, func(t *testing.T) {
			dir := initGitRepo(t)
			main, err := mainCheckout(context.Background(), dir)
			if err != nil {
				t.Fatal(err)
			}
			daemon := tuitest.NewMockDaemon(t)
			c := client.NewLocal(daemon.SocketPath())
			daemon.SetValidateRepoPathResult(&pb.ValidateRepoPathResponse{IsValid: true, IsGithub: true, OriginUrl: "https://github.com/owner/project.git", DefaultBranch: "develop"})
			if mode != "register" {
				daemon.AddRepo(&pb.Repo{Id: "existing", LocalPath: main})
			}
			target := dir
			if mode == "symlink" {
				target = filepath.Join(t.TempDir(), "alias")
				if err := os.Symlink(dir, target); err != nil {
					t.Fatal(err)
				}
			}
			if mode == "worktree" {
				target = filepath.Join(t.TempDir(), "linked")
				if b, err := exec.Command("git", "-C", dir, "worktree", "add", "-b", "linked", target).CombinedOutput(); err != nil {
					t.Fatalf("worktree: %v %s", err, b)
				}
			}
			steps := &initAppliedSteps{}
			repo, err := resolveInitRepo(context.Background(), c, target, true, steps)
			if err != nil {
				t.Fatal(err)
			}
			if repo.LocalPath != main {
				t.Fatalf("main checkout not resolved: %s", repo.LocalPath)
			}
			calls := daemon.RegisterRepoCalls()
			if mode == "register" {
				if len(calls) != 1 || calls[0].DisplayName != "@owner/project" || calls[0].DefaultBaseBranch != "develop" || !steps.Registered {
					t.Fatal("registration defaults incorrect")
				}
				_, err = resolveInitRepo(context.Background(), c, target, true, steps)
				if err != nil || len(daemon.RegisterRepoCalls()) != 1 {
					t.Fatal("registration not convergent")
				}
			} else if len(calls) != 0 {
				t.Fatal("registered existing repository")
			}
		})
	}
}

func TestInitRepoNonGitAndNoRegister(t *testing.T) {
	daemon := tuitest.NewMockDaemon(t)
	c := client.NewLocal(daemon.SocketPath())
	for _, tc := range []struct {
		dir, want string
		allow     bool
	}{{t.TempDir(), "git repository", true}, {initGitRepo(t), "--register", false}} {
		_, err := resolveInitRepo(context.Background(), c, tc.dir, tc.allow, &initAppliedSteps{})
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Fatal("expected actionable refusal")
		}
	}
	if len(daemon.RegisterRepoCalls()) != 0 {
		t.Fatal("unexpected registration")
	}
}

type staleInitClient struct{ client.BossClient }

func (c staleInitClient) UpdateRepo(context.Context, *pb.UpdateRepoRequest) (*pb.Repo, error) {
	return &pb.Repo{}, nil
}

type failedReadbackInitClient struct {
	client.BossClient
	mode string
}

func (c failedReadbackInitClient) ListRepos(context.Context) ([]*pb.Repo, error) {
	switch c.mode {
	case "failed":
		return nil, errors.New("transport failure containing new-linear-secret")
	case "missing":
		return nil, nil
	default:
		return []*pb.Repo{{Id: "repo", LinearApiKey: "old"}}, nil
	}
}

func TestInitRepoCredentialsAppliedButUnverified(t *testing.T) {
	for _, mode := range []string{"failed", "missing", "mismatch"} {
		t.Run(mode, func(t *testing.T) {
			daemon := tuitest.NewMockDaemon(t)
			daemon.AddRepo(&pb.Repo{Id: "repo", LinearApiKey: "old"})
			base := client.NewLocal(daemon.SocketPath())
			steps := &initAppliedSteps{}
			var out bytes.Buffer
			opts := initRepoOptions{linearKey: "new-linear-secret", sentryToken: "new-sentry-secret", sentryOrg: "org"}
			err := storeInitCredentials(context.Background(), failedReadbackInitClient{base, mode}, &pb.Repo{Id: "repo", LinearApiKey: "old"}, opts, nil, steps, &out)
			if err == nil || !strings.Contains(err.Error(), "update succeeded but could not be verified") {
				t.Fatalf("expected successful but unverified update: %v", err)
			}
			if !steps.LinearStored || !steps.SentryStored {
				t.Fatal("successful credential writes missing from applied steps")
			}
			if steps.LinearVerified || steps.SentryVerified {
				t.Fatal("failed read-back recorded as verified")
			}
			if out.Len() != 0 {
				t.Fatal("unverified write reported as success")
			}
			if strings.Contains(err.Error(), opts.linearKey) || strings.Contains(err.Error(), opts.sentryToken) {
				t.Fatal("credential leaked in error")
			}
			stored, readErr := base.ListRepos(context.Background())
			if readErr != nil || len(stored) != 1 || stored[0].GetLinearApiKey() != opts.linearKey || stored[0].GetSentryApiKey() != opts.sentryToken || stored[0].GetSentryOrg() != opts.sentryOrg {
				t.Fatal("applied credentials were rolled back or lost")
			}
			if len(daemon.UpdateRepoCalls()) != 1 {
				t.Fatal("verification failure must not roll back a successful write")
			}
		})
	}
}

func TestInitRepoCredentials(t *testing.T) {
	for _, mode := range []string{"linear", "sentry", "identical", "readback", "validator"} {
		t.Run(mode, func(t *testing.T) {
			old := "old"
			repo := &pb.Repo{Id: "repo", LinearApiKey: old}
			daemon := tuitest.NewMockDaemon(t)
			daemon.AddRepo(repo)
			base := client.NewLocal(daemon.SocketPath())
			var c client.BossClient = base
			opts := initRepoOptions{linearKey: "new-secret"}
			steps := &initAppliedSteps{}
			var out bytes.Buffer
			var validate func(context.Context, string) (string, error)
			switch mode {
			case "sentry":
				opts = initRepoOptions{sentryOrg: "org", sentryToken: "sentry-secret"}
			case "identical":
				opts.linearKey = old
			case "readback":
				c = staleInitClient{base}
			case "validator":
				validate = func(context.Context, string) (string, error) { return "", errors.New(opts.linearKey) }
			}
			err := storeInitCredentials(context.Background(), c, repo, opts, validate, steps, &out)
			if mode == "readback" || mode == "validator" {
				if err == nil {
					t.Fatal("expected failure")
				}
				if repo.GetLinearApiKey() != old {
					t.Fatal("old key overwritten")
				}
				if strings.Contains(err.Error(), opts.linearKey) {
					t.Fatal("secret leaked in error")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			calls := daemon.UpdateRepoCalls()
			if mode == "identical" {
				if len(calls) != 0 {
					t.Fatal("identical key updated")
				}
				return
			}
			if len(calls) != 1 || !strings.Contains(out.String(), "verified") {
				t.Fatal("missing update/readback")
			}
			if mode == "linear" {
				if calls[0].LinearKey.Action != pb.SecretAction_SECRET_ACTION_SET || !steps.LinearStored || !steps.LinearVerified || !strings.Contains(out.String(), "not validated") {
					t.Fatal("Linear update incorrect")
				}
			}
			if mode == "sentry" {
				if calls[0].SentryKey.Action != pb.SecretAction_SECRET_ACTION_SET || calls[0].GetSentryOrg() != "org" || !steps.SentryStored || !steps.SentryVerified {
					t.Fatal("Sentry update incorrect")
				}
			}
			if strings.Contains(out.String(), "secret") {
				t.Fatal("secret leaked")
			}
		})
	}
}

type unreadableInitInput struct{}

func (unreadableInitInput) Read([]byte) (int, error) { panic("stdin must not be read") }
func TestInitRepoUsageBeforeRead(t *testing.T) {
	for _, o := range []initRepoOptions{{linearStdin: true, sentryStdin: true}, {sentryOrg: "org"}, {sentryStdin: true}, {register: true, noRegister: true}, {sentryOrg: "org", linearStdin: true}} {
		if err := o.readCredentials(unreadableInitInput{}, func(string) string { return "" }); err == nil {
			t.Fatal("expected usage error")
		}
	}
}

func TestInitRepoStdinAndEnvironment(t *testing.T) {
	for _, tc := range []struct {
		o               initRepoOptions
		env, key, token string
	}{
		{initRepoOptions{linearStdin: true}, "", "stdin-secret", ""},
		{initRepoOptions{sentryStdin: true, sentryOrg: "org"}, "", "", "stdin-secret"},
		{initRepoOptions{}, "env-secret", "", ""},
		{initRepoOptions{storeEnv: true}, "env-secret", "env-secret", ""},
	} {
		o := tc.o
		err := o.readCredentials(strings.NewReader("stdin-secret\n"), func(name string) string {
			if name == "LINEAR_API_KEY" {
				return tc.env
			}
			return ""
		})
		if err != nil || o.linearKey != tc.key || o.sentryToken != tc.token {
			t.Fatal("credential source selection failed")
		}
	}
}

func TestInitRepoRemoteAndHost(t *testing.T) {
	for _, flag := range []string{"remote", "host"} {
		t.Run(flag, func(t *testing.T) {
			var out bytes.Buffer
			c := &cobra.Command{}
			c.Flags().String(flag, "remote-destination", "")
			c.SetOut(&out)
			c.SetIn(unreadableInitInput{})
			repo, err := prepareInitRepo(c, t.TempDir(), initRepoOptions{register: true, linearStdin: true}, nil, &initAppliedSteps{})
			if err != nil || repo != nil || !strings.Contains(out.String(), "local path") {
				t.Fatal("remote path not skipped")
			}
		})
	}
}

func TestInitRepoConfigLocationWarning(t *testing.T) {
	dir := initGitRepo(t)
	alias := filepath.Join(t.TempDir(), "alias")
	if err := os.Symlink(dir, alias); err != nil {
		t.Fatal(err)
	}
	daemon := tuitest.NewMockDaemon(t)
	daemon.AddRepo(&pb.Repo{Id: "existing", LocalPath: alias})
	t.Setenv("BOSS_SOCKET", daemon.SocketPath())
	linked := filepath.Join(t.TempDir(), "linked")
	if b, err := exec.Command("git", "-C", dir, "worktree", "add", "-b", "linked", linked).CombinedOutput(); err != nil {
		t.Fatalf("worktree: %v %s", err, b)
	}
	for _, target := range []string{alias, linked} {
		var out bytes.Buffer
		c := &cobra.Command{}
		c.SetContext(context.Background())
		c.SetOut(&out)
		_, err := prepareInitRepo(c, target, initRepoOptions{register: true}, nil, &initAppliedSteps{})
		if err != nil {
			t.Fatal(err)
		}
		warned := strings.Contains(out.String(), "cron gate reads the main checkout")
		if warned != (target == linked) {
			t.Fatal("config location warning did not compare canonical paths")
		}
	}
}

func TestInitRepoCommandUsageBeforeConfig(t *testing.T) {
	for _, flags := range [][]string{{"--linear-key-stdin", "--sentry-token-stdin"}, {"--sentry-org", "org"}, {"--sentry-token-stdin"}} {
		t.Setenv("LINEAR_API_KEY", "")
		t.Setenv("SENTRY_AUTH_TOKEN", "")
		c := initCmd()
		c.SetIn(unreadableInitInput{})
		c.SetOut(&bytes.Buffer{})
		c.SetErr(&bytes.Buffer{})
		// An inaccessible directory would fail first if argument validation came after detection.
		c.SetArgs(append([]string{"--dir", filepath.Join(t.TempDir(), "missing")}, flags...))
		err := c.Execute()
		if err == nil || strings.Contains(err.Error(), "cannot access") {
			t.Fatal("usage validation did not precede config inspection")
		}
	}
}
