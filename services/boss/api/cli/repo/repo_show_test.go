package repo

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/recurser/boss/internal/adapter/cloudrpc"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/spf13/cobra"
)

type repoReadClient struct {
	Client
	repos []*pb.Repo
}

func (c repoReadClient) ListRepos(context.Context) ([]*pb.Repo, error) { return c.repos, nil }

func TestRepoShow_Repos_AC4(t *testing.T) {
	for _, trusted := range []bool{false, true} {
		for _, asJSON := range []bool{false, true} {
			t.Run(strings.Join([]string{map[bool]string{true: "trusted", false: "untrusted"}[trusted], map[bool]string{true: "json", false: "text"}[asJSON]}, "/"), func(t *testing.T) {
				r := &pb.Repo{Id: "repo-1", DisplayName: "demo", LocalPath: "/demo", OriginUrl: "https://example.com/demo", DefaultBaseBranch: "main", IsTrusted: trusted, LinearApiKey: "linear-secret-sentinel", SentryApiKey: "sentry-secret-sentinel", SentryOrg: "org"}
				g := New(func(*cobra.Command) (Client, error) { return repoReadClient{repos: []*pb.Repo{r}}, nil }, Options{})
				cmd := g.RepoCommand()
				var out bytes.Buffer
				cmd.SetOut(&out)
				args := []string{"show", "repo-1"}
				if asJSON {
					args = append(args, "--json")
				}
				cmd.SetArgs(args)
				if err := cmd.Execute(); err != nil {
					t.Fatal(err)
				}
				got := out.String()
				for _, secret := range []string{r.LinearApiKey, r.SentryApiKey} {
					if strings.Contains(got, secret) {
						t.Fatal("secret leaked")
					}
				}
				if asJSON {
					var result map[string]any
					if err := json.Unmarshal(out.Bytes(), &result); err != nil {
						t.Fatal(err)
					}
					for key, want := range map[string]any{"id": "repo-1", "localPath": "/demo", "originUrl": r.OriginUrl, "defaultBaseBranch": "main", "hasLinearApiKey": true, "hasSentryAuthToken": true, "isTrusted": trusted, "canAutoRepair": false} {
						if result[key] != want {
							t.Errorf("%s = %v, want %v", key, result[key], want)
						}
					}
				} else {
					for _, want := range []string{"Trusted:", "Codex trust is written per worktree", "Claude loads project MCP servers", "/demo", r.OriginUrl, "main", "Linear API key:", "set", "Sentry org:"} {
						if !strings.Contains(got, want) {
							t.Errorf("missing %q in %s", want, got)
						}
					}
				}
			})
		}
	}
}

func TestRepoShowUnknown(t *testing.T) {
	g := New(func(*cobra.Command) (Client, error) { return repoReadClient{}, nil }, Options{})
	cmd := g.RepoCommand()
	cmd.SetArgs([]string{"show", "missing"})
	err := cmd.Execute()
	if err == nil || !strings.Contains(err.Error(), "missing") || !strings.Contains(err.Error(), "boss repo ls") {
		t.Fatalf("error = %v", err)
	}
}

func TestRepoListTrust_Repos_AC5(t *testing.T) {
	g := New(func(*cobra.Command) (Client, error) {
		return repoReadClient{repos: []*pb.Repo{{Id: "one", DisplayName: "trusted-repo", IsTrusted: true}, {Id: "two", DisplayName: "untrusted-repo"}}}, nil
	}, Options{})
	cmd := g.RepoCommand()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetArgs([]string{"ls"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"TRUSTED", "yes", "no"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("missing %q in %s", want, out.String())
		}
	}
}

func TestRepoSettingsRemoteRedactsTrustAndSecrets(t *testing.T) {
	c := cloudrpc.NewRemote("https://bosso.invalid", "token")
	r := &pb.Repo{Id: "r", IsTrusted: true, LinearApiKey: "linear-secret-sentinel", SentryApiKey: "sentry-secret-sentinel"}
	var out bytes.Buffer
	printUpdatedRepo(&out, c, r)
	if strings.Contains(out.String(), "Trusted") || strings.Contains(out.String(), "secret-sentinel") {
		t.Fatal("remote text revealed trust or secrets")
	}
	data, err := json.Marshal(repoSettingsFor(c, r))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "isTrusted") || strings.Contains(string(data), "secret-sentinel") {
		t.Fatalf("remote JSON revealed trust or secrets: %s", data)
	}
}
