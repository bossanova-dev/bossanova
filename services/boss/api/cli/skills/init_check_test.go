package skills

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"testing"
	"time"

	daemonrpc "github.com/recurser/boss/internal/adapter/daemonrpc"
	"github.com/recurser/boss/internal/domain/rpcerr"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/kit/bossmcp"
	"github.com/spf13/cobra"
	"google.golang.org/protobuf/types/known/timestamppb"
)

type setupCheckClientFake struct {
	Client
	listSetupChecks func(context.Context, *pb.ListSetupChecksRequest) (*pb.ListSetupChecksResponse, error)
}

func (f *setupCheckClientFake) ListSetupChecks(ctx context.Context, req *pb.ListSetupChecksRequest) (*pb.ListSetupChecksResponse, error) {
	return f.listSetupChecks(ctx, req)
}

type setupCheckWireUnavailableError struct{ msg string }

func (e setupCheckWireUnavailableError) Error() string        { return e.msg }
func (e setupCheckWireUnavailableError) RPCKind() rpcerr.Kind { return rpcerr.Unavailable }
func (e setupCheckWireUnavailableError) RPCWireError() bool   { return true }

func sampleSetupCheckReport() *pb.ListSetupChecksResponse {
	return &pb.ListSetupChecksResponse{
		Checks: []*pb.SetupCheck{
			{
				Id:             "daemon-running",
				Area:           pb.SetupCheckArea_SETUP_CHECK_AREA_MACHINE,
				Status:         pb.SetupCheckStatus_SETUP_CHECK_STATUS_OK,
				Title:          "Bossanova daemon running",
				Evidence:       "bossd daemon is running and answering RPCs",
				AddedInVersion: setupCheckAddedInVersion,
			},
			{
				Id:             "repo-registered",
				Area:           pb.SetupCheckArea_SETUP_CHECK_AREA_REPO,
				Status:         pb.SetupCheckStatus_SETUP_CHECK_STATUS_OK,
				Title:          "Repository registered with bossd",
				Evidence:       "registered as repo-1 (/work/repo)",
				AddedInVersion: setupCheckAddedInVersion,
			},
			{
				Id:             "gh-auth",
				Area:           pb.SetupCheckArea_SETUP_CHECK_AREA_MACHINE,
				Status:         pb.SetupCheckStatus_SETUP_CHECK_STATUS_MISSING,
				Title:          "GitHub CLI authenticated in daemon environment",
				Evidence:       "bossd's gh CLI is not authenticated to GitHub",
				Recommendation: "Authenticate gh in the daemon environment so PR creation, checks and merges succeed.",
				Fix: &pb.SetupCheckFix{
					Kind:    pb.SetupCheckFixKind_SETUP_CHECK_FIX_KIND_COMMAND,
					Command: "gh auth login",
				},
				AddedInVersion: setupCheckAddedInVersion,
			},
			{
				Id:             "node-on-daemon-path",
				Area:           pb.SetupCheckArea_SETUP_CHECK_AREA_MACHINE,
				Status:         pb.SetupCheckStatus_SETUP_CHECK_STATUS_BLOCKED,
				Title:          "Node.js on daemon PATH",
				Evidence:       "unable to probe node on bossd PATH",
				Recommendation: "Verify that bossd can search its service PATH for node.",
				Fix: &pb.SetupCheckFix{
					Kind:      pb.SetupCheckFixKind_SETUP_CHECK_FIX_KIND_AGENT_TASK,
					AgentTask: "Inspect the daemon service PATH and daemon_path_extra so bossd can resolve node.",
				},
				AddedInVersion: setupCheckAddedInVersion,
			},
			{
				Id:             "skills-current",
				Area:           pb.SetupCheckArea_SETUP_CHECK_AREA_MACHINE,
				Status:         pb.SetupCheckStatus_SETUP_CHECK_STATUS_OUTDATED,
				Title:          "Installed skills match daemon version",
				Evidence:       "installed skills v1.197.0 do not match daemon v1.198.0",
				Recommendation: "Sync installed skills so they match the running daemon version.",
				Fix: &pb.SetupCheckFix{
					Kind:    pb.SetupCheckFixKind_SETUP_CHECK_FIX_KIND_COMMAND,
					Command: "boss skills sync",
				},
				AddedInVersion: setupCheckAddedInVersion,
			},
		},
		CheckedAt: timestamppb.New(time.Date(2026, 11, 24, 19, 30, 0, 0, time.UTC)),
		RepoId:    "repo-1",
	}
}

func runInitSubtree(t *testing.T, g *Group, remote string, args ...string) (string, error) {
	t.Helper()
	root := &cobra.Command{Use: "boss", SilenceUsage: true, SilenceErrors: true}
	root.PersistentFlags().String("remote", remote, "")
	root.PersistentFlags().String("host", "", "")
	root.AddCommand(g.initCmd())

	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&bytes.Buffer{})
	root.SetArgs(args)
	err := root.Execute()
	return out.String(), err
}

func TestInitCheckTextAndJSONGolden_BossInit_AC268301(t *testing.T) {
	dir := t.TempDir()
	resp := sampleSetupCheckReport()

	var gotReq *pb.ListSetupChecksRequest
	g := New(
		func(*cobra.Command) (Client, error) {
			t.Fatal("default newClient should not be called when NewSetupCheckClient is configured")
			return nil, nil
		},
		Config{
			NewSetupCheckClient: func(*cobra.Command) (Client, error) {
				return &setupCheckClientFake{
					listSetupChecks: func(_ context.Context, req *pb.ListSetupChecksRequest) (*pb.ListSetupChecksResponse, error) {
						gotReq = req
						return resp, nil
					},
				}, nil
			},
		},
	)

	t.Run("text golden output exits 0 even with missing, blocked, and outdated checks", func(t *testing.T) {
		got, err := runInitSubtree(t, g, "", "init", "check", "--dir", dir)
		if err != nil {
			t.Fatalf("boss init check returned error %v, want exit 0", err)
		}
		absDir, err := filepath.Abs(dir)
		if err != nil {
			t.Fatal(err)
		}
		if gotReq.GetWorkspacePath() != absDir {
			t.Fatalf("WorkspacePath = %q, want %q", gotReq.GetWorkspacePath(), absDir)
		}

		want := strings.Join([]string{
			"3 of 5 checks need attention",
			"OK  daemon-running  Bossanova daemon running",
			"OK  repo-registered  Repository registered with bossd",
			"MISSING  gh-auth  GitHub CLI authenticated in daemon environment",
			"  evidence: bossd's gh CLI is not authenticated to GitHub",
			"  recommendation: Authenticate gh in the daemon environment so PR creation, checks and merges succeed.",
			"  fix: gh auth login",
			"BLOCKED  node-on-daemon-path  Node.js on daemon PATH",
			"  evidence: unable to probe node on bossd PATH",
			"  recommendation: Verify that bossd can search its service PATH for node.",
			"  fix: Inspect the daemon service PATH and daemon_path_extra so bossd can resolve node.",
			"OUTDATED  skills-current  Installed skills match daemon version",
			"  evidence: installed skills v1.197.0 do not match daemon v1.198.0",
			"  recommendation: Sync installed skills so they match the running daemon version.",
			"  fix: boss skills sync",
			"",
		}, "\n")
		if got != want {
			t.Fatalf("text output mismatch:\ngot:\n%s\nwant:\n%s", got, want)
		}
	})

	t.Run("prepends daemon-running OK when daemon omits it", func(t *testing.T) {
		withoutDaemon := &pb.ListSetupChecksResponse{
			Checks: []*pb.SetupCheck{
				{
					Id:             "gh-auth",
					Area:           pb.SetupCheckArea_SETUP_CHECK_AREA_MACHINE,
					Status:         pb.SetupCheckStatus_SETUP_CHECK_STATUS_OK,
					Title:          "GitHub CLI authenticated",
					Evidence:       "ok",
					AddedInVersion: setupCheckAddedInVersion,
				},
			},
			CheckedAt: timestamppb.New(time.Date(2026, 11, 24, 19, 30, 0, 0, time.UTC)),
		}
		g2 := New(func(*cobra.Command) (Client, error) {
			return &setupCheckClientFake{
				listSetupChecks: func(context.Context, *pb.ListSetupChecksRequest) (*pb.ListSetupChecksResponse, error) {
					return withoutDaemon, nil
				},
			}, nil
		}, Config{})

		got, err := runInitSubtree(t, g2, "", "init", "check", "--dir", dir)
		if err != nil {
			t.Fatalf("boss init check: %v", err)
		}
		want := strings.Join([]string{
			"0 of 2 checks need attention",
			"OK  daemon-running  Bossanova daemon running",
			"OK  gh-auth  GitHub CLI authenticated",
			"",
		}, "\n")
		if got != want {
			t.Fatalf("text output mismatch:\ngot:\n%s\nwant:\n%s", got, want)
		}
		if len(withoutDaemon.Checks) != 1 {
			t.Fatalf("daemon response was mutated in place: len(Checks) = %d, want 1", len(withoutDaemon.Checks))
		}
	})

	t.Run("renders RECOMMENDED and DECLINED statuses", func(t *testing.T) {
		extra := &pb.ListSetupChecksResponse{
			Checks: []*pb.SetupCheck{
				{
					Id:             "daemon-running",
					Area:           pb.SetupCheckArea_SETUP_CHECK_AREA_MACHINE,
					Status:         pb.SetupCheckStatus_SETUP_CHECK_STATUS_OK,
					Title:          "Bossanova daemon running",
					AddedInVersion: setupCheckAddedInVersion,
				},
				{
					Id:             "notifications",
					Area:           pb.SetupCheckArea_SETUP_CHECK_AREA_MACHINE,
					Status:         pb.SetupCheckStatus_SETUP_CHECK_STATUS_RECOMMENDED,
					Title:          "Desktop notifications",
					Evidence:       "disabled",
					Recommendation: "Enable notifications.",
					Fix:            &pb.SetupCheckFix{Kind: pb.SetupCheckFixKind_SETUP_CHECK_FIX_KIND_COMMAND, Command: "boss settings set notifications true"},
					AddedInVersion: setupCheckAddedInVersion,
				},
				{
					Id:             "sentry",
					Area:           pb.SetupCheckArea_SETUP_CHECK_AREA_REPO,
					Status:         pb.SetupCheckStatus_SETUP_CHECK_STATUS_DECLINED,
					Title:          "Sentry configured",
					Evidence:       "declined by operator",
					Recommendation: "Configure Sentry.",
					Fix:            &pb.SetupCheckFix{Kind: pb.SetupCheckFixKind_SETUP_CHECK_FIX_KIND_AGENT_TASK, AgentTask: "Add sentry_org."},
					AddedInVersion: setupCheckAddedInVersion,
				},
			},
			CheckedAt: timestamppb.Now(),
		}
		g2 := New(func(*cobra.Command) (Client, error) {
			return &setupCheckClientFake{
				listSetupChecks: func(context.Context, *pb.ListSetupChecksRequest) (*pb.ListSetupChecksResponse, error) {
					return extra, nil
				},
			}, nil
		}, Config{})
		got, err := runInitSubtree(t, g2, "", "init", "check", "--dir", dir)
		if err != nil {
			t.Fatalf("boss init check: %v", err)
		}
		if !strings.Contains(got, "2 of 3 checks need attention\n") ||
			!strings.Contains(got, "RECOMMENDED  notifications  Desktop notifications\n") ||
			!strings.Contains(got, "DECLINED  sentry  Sentry configured\n") {
			t.Fatalf("unexpected status rendering:\n%s", got)
		}
	})
}

func TestInitCheckJSONMatchesBossMCP_McpServer_AC268301(t *testing.T) {
	dir := t.TempDir()
	resp := sampleSetupCheckReport()

	g := New(func(*cobra.Command) (Client, error) {
		return &setupCheckClientFake{
			listSetupChecks: func(context.Context, *pb.ListSetupChecksRequest) (*pb.ListSetupChecksResponse, error) {
				return resp, nil
			},
		}, nil
	}, Config{})

	cliOut, err := runInitSubtree(t, g, "", "init", "check", "--dir", dir, "--json")
	if err != nil {
		t.Fatalf("boss init check --json: %v", err)
	}
	mcpBytes, err := bossmcp.MarshalSetupChecks(resp)
	if err != nil {
		t.Fatalf("MarshalSetupChecks: %v", err)
	}
	if !bytes.Equal([]byte(cliOut), mcpBytes) {
		t.Fatalf("CLI --json bytes differ from bossmcp.MarshalSetupChecks:\nCLI:\n%s\nMCP:\n%s", cliOut, mcpBytes)
	}
	if !strings.HasSuffix(cliOut, "\n") {
		t.Fatalf("CLI --json output must end with a newline, got %q", cliOut)
	}
}

func hashDirectoryTree(t *testing.T, root string) string {
	t.Helper()
	var entries []string
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		if d.IsDir() {
			entries = append(entries, "D:"+rel)
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		sum := sha256.Sum256(data)
		entries = append(entries, "F:"+rel+":"+hex.EncodeToString(sum[:]))
		return nil
	})
	if err != nil {
		t.Fatalf("WalkDir(%s): %v", root, err)
	}
	sort.Strings(entries)
	h := sha256.Sum256([]byte(strings.Join(entries, "\n")))
	return hex.EncodeToString(h[:])
}

func TestInitCheckReadOnlyAndDeterministic_BossInit_AC268302(t *testing.T) {
	dir := t.TempDir()
	writeFixture(t, dir, map[string]string{
		".boss-skills.json":  "{\n  \"commands\": {\"test\": \"go test ./...\"}\n}\n",
		".boss-project.json": "{\n  \"version\": 1\n}\n",
		"sub/nested.txt":     "keep untouched\n",
	})
	beforeHash := hashDirectoryTree(t, dir)

	timestamps := []time.Time{
		time.Date(2026, 11, 24, 19, 0, 0, 0, time.UTC),
		time.Date(2026, 11, 24, 19, 1, 0, 0, time.UTC),
		time.Date(2026, 11, 24, 19, 2, 0, 0, time.UTC),
	}
	call := 0
	g := New(func(*cobra.Command) (Client, error) {
		return &setupCheckClientFake{
			listSetupChecks: func(context.Context, *pb.ListSetupChecksRequest) (*pb.ListSetupChecksResponse, error) {
				r := sampleSetupCheckReport()
				r.CheckedAt = timestamppb.New(timestamps[call])
				call++
				return r, nil
			},
		}, nil
	}, Config{})

	if _, err := runInitSubtree(t, g, "", "init", "check", "--dir", dir); err != nil {
		t.Fatalf("text run: %v", err)
	}
	json1, err := runInitSubtree(t, g, "", "init", "check", "--dir", dir, "--json")
	if err != nil {
		t.Fatalf("first json run: %v", err)
	}
	json2, err := runInitSubtree(t, g, "", "init", "check", "--dir", dir, "--json")
	if err != nil {
		t.Fatalf("second json run: %v", err)
	}

	afterHash := hashDirectoryTree(t, dir)
	if beforeHash != afterHash {
		t.Fatalf("directory tree hash changed: before=%s after=%s", beforeHash, afterHash)
	}

	var doc1, doc2 map[string]any
	if err := json.Unmarshal([]byte(json1), &doc1); err != nil {
		t.Fatalf("unmarshal json1: %v", err)
	}
	if err := json.Unmarshal([]byte(json2), &doc2); err != nil {
		t.Fatalf("unmarshal json2: %v", err)
	}
	if doc1["checked_at"] == doc2["checked_at"] {
		t.Fatalf("expected checked_at to differ between runs, both were %v", doc1["checked_at"])
	}
	checks1, _ := json.Marshal(doc1["checks"])
	checks2, _ := json.Marshal(doc2["checks"])
	if !bytes.Equal(checks1, checks2) {
		t.Fatalf("checks differed across identical runs:\nrun1: %s\nrun2: %s", checks1, checks2)
	}
}

func TestInitCheckUnreachableDaemon_BossInit_AC268303(t *testing.T) {
	dir := t.TempDir()
	socketPath := filepath.Join(t.TempDir(), "missing-bossd.sock")

	for _, tc := range []struct {
		name      string
		installed bool
		wantFix   string
		clientErr error
	}{
		{
			name:      "service installed suggests boss daemon start",
			installed: true,
			wantFix:   "boss daemon start",
			clientErr: rpcerr.New(rpcerr.Unavailable, errors.New("dial unix: connect: no such file or directory")),
		},
		{
			name:      "service not installed suggests boss daemon install",
			installed: false,
			wantFix:   "boss daemon install",
			clientErr: syscall.ECONNREFUSED,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := New(func(*cobra.Command) (Client, error) {
				return &setupCheckClientFake{
					listSetupChecks: func(context.Context, *pb.ListSetupChecksRequest) (*pb.ListSetupChecksResponse, error) {
						return nil, tc.clientErr
					},
				}, nil
			}, Config{
				SetupCheckSocketPath:       func() (string, error) { return socketPath, nil },
				SetupCheckServiceInstalled: func() bool { return tc.installed },
			})

			textOut, err := runInitSubtree(t, g, "", "init", "check", "--dir", dir)
			if err != nil {
				t.Fatalf("text unreachable daemon returned error %v, want exit 0", err)
			}
			for _, want := range []string{
				"1 of 1 checks need attention",
				"MISSING  daemon-running  Bossanova daemon running",
				"  evidence: cannot reach bossd daemon at " + socketPath,
				"  recommendation: Start the Bossanova daemon; the remaining setup checks run once the daemon is up.",
				"  fix: " + tc.wantFix,
			} {
				if !strings.Contains(textOut, want) {
					t.Errorf("text output missing %q:\n%s", want, textOut)
				}
			}

			jsonOut, err := runInitSubtree(t, g, "", "init", "check", "--dir", dir, "--json")
			if err != nil {
				t.Fatalf("json unreachable daemon returned error %v, want exit 0", err)
			}
			var decoded struct {
				Checks []struct {
					ID             string `json:"id"`
					Area           string `json:"area"`
					Status         string `json:"status"`
					Title          string `json:"title"`
					Evidence       string `json:"evidence"`
					Recommendation string `json:"recommendation"`
					AddedInVersion string `json:"added_in_version"`
					Fix            struct {
						Kind    string `json:"kind"`
						Command string `json:"command"`
					} `json:"fix"`
				} `json:"checks"`
				CheckedAt string `json:"checked_at"`
			}
			if err := json.Unmarshal([]byte(jsonOut), &decoded); err != nil {
				t.Fatalf("unmarshal json: %v\n%s", err, jsonOut)
			}
			if len(decoded.Checks) != 1 {
				t.Fatalf("got %d checks, want 1", len(decoded.Checks))
			}
			c := decoded.Checks[0]
			if c.ID != "daemon-running" ||
				c.Area != "SETUP_CHECK_AREA_MACHINE" ||
				c.Status != "SETUP_CHECK_STATUS_MISSING" ||
				!strings.Contains(c.Evidence, socketPath) ||
				c.Fix.Kind != "SETUP_CHECK_FIX_KIND_COMMAND" ||
				c.Fix.Command != tc.wantFix ||
				c.AddedInVersion != setupCheckAddedInVersion ||
				setupCheckAddedInVersion != "v1.202.0" ||
				decoded.CheckedAt == "" {
				t.Fatalf("unexpected unreachable check JSON: %+v (checked_at=%q)", c, decoded.CheckedAt)
			}
		})
	}

	t.Run("real daemonrpc.LocalClient against absent Unix socket exits 0 with daemon-running MISSING", func(t *testing.T) {
		g := New(func(*cobra.Command) (Client, error) {
			return daemonrpc.NewLocal(socketPath), nil
		}, Config{
			SetupCheckSocketPath:       func() (string, error) { return socketPath, nil },
			SetupCheckServiceInstalled: func() bool { return true },
		})

		out, err := runInitSubtree(t, g, "", "init", "check", "--dir", dir)
		if err != nil {
			t.Fatalf("real LocalClient against absent socket returned error %v, want exit 0", err)
		}
		if !strings.Contains(out, "MISSING  daemon-running  Bossanova daemon running") || !strings.Contains(out, socketPath) {
			t.Fatalf("unexpected output:\n%s", out)
		}
	})
}

func TestInitCheckUnexpectedErrorsAndUsageErrors(t *testing.T) {
	dir := t.TempDir()

	t.Run("unexpected RPC errors return non-zero", func(t *testing.T) {
		for _, tc := range []struct {
			name   string
			remote string
			host   string
			err    error
		}{
			{
				name: "unimplemented",
				err:  rpcerr.New(rpcerr.Unimplemented, errors.New("setup checks are host-local; run boss init check on the host")),
			},
			{
				name: "internal",
				err:  rpcerr.New(rpcerr.Internal, errors.New("db exploded")),
			},
			{
				name: "wire unavailable from reachable server",
				err:  setupCheckWireUnavailableError{msg: "server returned unavailable"},
			},
			{
				name:   "unavailable under --remote is not treated as missing local daemon",
				remote: "https://orchestrator.example.test",
				err:    rpcerr.New(rpcerr.Unavailable, errors.New("remote down")),
			},
			{
				name: "unavailable under --host is not treated as missing local daemon",
				host: "user@example.test",
				err:  rpcerr.New(rpcerr.Unavailable, errors.New("ssh tunnel closed")),
			},
			{
				name: "connection refused under --host is not treated as missing local daemon",
				host: "user@example.test",
				err:  syscall.ECONNREFUSED,
			},
		} {
			t.Run(tc.name, func(t *testing.T) {
				g := New(func(*cobra.Command) (Client, error) {
					return &setupCheckClientFake{
						listSetupChecks: func(context.Context, *pb.ListSetupChecksRequest) (*pb.ListSetupChecksResponse, error) {
							return nil, tc.err
						},
					}, nil
				}, Config{})
				args := []string{"init", "check", "--dir", dir}
				if tc.host != "" {
					args = append([]string{"--host", tc.host}, args...)
				}
				if _, err := runInitSubtree(t, g, tc.remote, args...); err == nil {
					t.Fatalf("expected error for %s, got nil", tc.name)
				}
			})
		}
	})

	t.Run("usage errors on bad args and --dir", func(t *testing.T) {
		g := New(func(*cobra.Command) (Client, error) {
			t.Fatal("client should not be dialed on usage error")
			return nil, nil
		}, Config{})

		if _, err := runInitSubtree(t, g, "", "init", "check", "extra-arg"); err == nil {
			t.Fatal("expected error for positional argument to boss init check")
		}
		if _, err := runInitSubtree(t, g, "", "init", "check", "--dir", filepath.Join(dir, "does-not-exist")); err == nil {
			t.Fatal("expected error for nonexistent --dir")
		}
		filePath := filepath.Join(dir, "not-a-dir")
		if err := os.WriteFile(filePath, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := runInitSubtree(t, g, "", "init", "check", "--dir", filePath); err == nil {
			t.Fatal("expected error when --dir points to a file")
		}
	})

	t.Run("bare boss init still rejects positional arguments and routes flags unchanged", func(t *testing.T) {
		g := newTestGroup()
		if _, err := runInitSubtree(t, g, "", "init", "../other-repo"); err == nil {
			t.Fatal("expected bare boss init with positional arg to fail")
		}
		initDir := t.TempDir()
		writeFixture(t, initDir, map[string]string{"go.mod": "module example.com/fx\n\ngo 1.25\n"})
		out, err := runInitSubtree(t, g, "", "init", "--dir", initDir)
		if err != nil {
			t.Fatalf("boss init --dir: %v", err)
		}
		if !strings.Contains(out, ".boss-skills.json") {
			t.Fatalf("boss init output did not mention .boss-skills.json:\n%s", out)
		}
		if _, err := os.Stat(filepath.Join(initDir, ".boss-skills.json")); err != nil {
			t.Fatalf("boss init did not write .boss-skills.json: %v", err)
		}
	})
}

func TestInitCheckHostWorkspacePath(t *testing.T) {
	t.Run("omitting --dir under --host sends empty WorkspacePath instead of local cwd", func(t *testing.T) {
		var gotReq *pb.ListSetupChecksRequest
		g := New(func(*cobra.Command) (Client, error) {
			return &setupCheckClientFake{
				listSetupChecks: func(_ context.Context, req *pb.ListSetupChecksRequest) (*pb.ListSetupChecksResponse, error) {
					gotReq = req
					return sampleSetupCheckReport(), nil
				},
			}, nil
		}, Config{})

		if _, err := runInitSubtree(t, g, "", "--host", "user@example.test", "init", "check"); err != nil {
			t.Fatalf("boss --host init check: %v", err)
		}
		if gotReq == nil {
			t.Fatal("ListSetupChecks was not called")
		}
		if gotReq.GetWorkspacePath() != "" {
			t.Fatalf("WorkspacePath = %q, want empty when --dir omitted under --host", gotReq.GetWorkspacePath())
		}
	})

	t.Run("passing remote --dir under --host forwards it without local stat", func(t *testing.T) {
		remotePath := filepath.Join(t.TempDir(), "nonexistent-locally", "remote-repo")
		var gotReq *pb.ListSetupChecksRequest
		g := New(func(*cobra.Command) (Client, error) {
			return &setupCheckClientFake{
				listSetupChecks: func(_ context.Context, req *pb.ListSetupChecksRequest) (*pb.ListSetupChecksResponse, error) {
					gotReq = req
					return sampleSetupCheckReport(), nil
				},
			}, nil
		}, Config{})

		if _, err := runInitSubtree(t, g, "", "--host", "user@example.test", "init", "check", "--dir", remotePath); err != nil {
			t.Fatalf("boss --host init check --dir %s: %v", remotePath, err)
		}
		if gotReq == nil {
			t.Fatal("ListSetupChecks was not called")
		}
		if gotReq.GetWorkspacePath() != remotePath {
			t.Fatalf("WorkspacePath = %q, want %q", gotReq.GetWorkspacePath(), remotePath)
		}
	})

	t.Run("passing relative --dir under --host fails before dialing client", func(t *testing.T) {
		g := New(func(*cobra.Command) (Client, error) {
			t.Fatal("client should not be dialed for relative --dir under --host")
			return nil, nil
		}, Config{})

		if _, err := runInitSubtree(t, g, "", "--host", "user@example.test", "init", "check", "--dir", "relative/repo"); err == nil {
			t.Fatal("expected error for relative --dir under --host")
		}
	})
}
