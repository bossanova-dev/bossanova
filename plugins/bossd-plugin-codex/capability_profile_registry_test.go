package main

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	sharedplugin "github.com/recurser/bossalib/contract/plugin/plugin"
)

func TestCodexAppServerOperationRegistryUsesRunnerLoginShell(t *testing.T) {
	var gotName string
	var gotArgs []string
	var launched *exec.Cmd
	registry := codexAppServerOperationRegistry{
		binary:     "codex",
		loginShell: "/bin/sh",
		commandFactory: func(ctx context.Context, name string, args ...string) *exec.Cmd {
			gotName, gotArgs = name, append([]string(nil), args...)
			launched = exec.CommandContext(ctx, "/bin/sh", "-c", `
				read -r initialize
				printf '%s\n' '{"id":1,"result":{}}'
				read -r initialized
				read -r list
				printf '%s\n' '{"id":2,"result":{"data":[]}}'
			`)
			return launched
		},
	}
	_, err := registry.Operations(context.Background(), codexRuntimeTarget{
		Model:    "gpt-profiled",
		ExtraEnv: map[string]string{"CODEX_HOME": "/projected/home"},
	})
	if err != nil {
		t.Fatalf("Operations: %v", err)
	}
	want := []string{"-l", "-c", "exec \"$@\"", "sh", "codex", "app-server", "--stdio", "-c", `model="gpt-profiled"`}
	if gotName != "/bin/sh" || strings.Join(gotArgs, "\x00") != strings.Join(want, "\x00") {
		t.Fatalf("preflight argv = %q %q, want %q %q", gotName, gotArgs, "/bin/sh", want)
	}
	if launched == nil || !strings.Contains(strings.Join(launched.Env, "\x00"), "CODEX_HOME=/projected/home") {
		t.Fatalf("preflight command did not receive projected CODEX_HOME: %v", launched)
	}
}

func TestCodexAppServerOperationRegistryWaitsForInitialized(t *testing.T) {
	t.Setenv("CODEX_REGISTRY_FIXTURE_MODE", "strict-handshake")
	registry := codexAppServerOperationRegistry{
		binary:         "codex",
		commandFactory: registryFixtureCommand,
	}

	if _, err := registry.Operations(context.Background(), codexRuntimeTarget{}); err != nil {
		t.Fatalf("Operations: %v", err)
	}
}

func TestReadAppServerResponseValidatesIDAndError(t *testing.T) {
	tests := []struct {
		name       string
		response   string
		expectedID int
		want       string
	}{
		{
			name:       "response id",
			response:   `{"id":2,"result":{}}`,
			expectedID: 1,
			want:       "response id = 2, want 1",
		},
		{
			name:       "JSON-RPC error",
			response:   `{"id":2,"error":{"code":-32000,"message":"registry unavailable"}}`,
			expectedID: 2,
			want:       "response id 2 error -32000: registry unavailable",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := readAppServerResponse(json.NewDecoder(strings.NewReader(tc.response)), tc.expectedID)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("readAppServerResponse error = %v, want containing %q", err, tc.want)
			}
		})
	}
}

func TestCodexAppServerOperationRegistryAcceptsResponseLargerThanScannerLimit(t *testing.T) {
	t.Setenv("CODEX_REGISTRY_FIXTURE_MODE", "large-response")
	registry := codexAppServerOperationRegistry{
		binary:         "codex",
		commandFactory: registryFixtureCommand,
	}

	surface, err := registry.Operations(context.Background(), codexRuntimeTarget{})
	if err != nil {
		t.Fatalf("Operations: %v", err)
	}
	if len(surface.Servers) != 1 || surface.Servers[0].Name != "large-fixture" {
		t.Fatalf("servers = %+v, want large-fixture", surface.Servers)
	}
}

func TestPreflightAggregatesPaginatedRuntimeOperationRegistry(t *testing.T) {
	t.Setenv("CODEX_REGISTRY_FIXTURE_MODE", "paginated")
	home := t.TempDir()
	srv := newTestServer(t)
	srv.operationRegistry = codexAppServerOperationRegistry{
		binary:         "codex",
		commandFactory: registryFixtureCommand,
	}

	if _, err := srv.PreflightHeadlessRun(context.Background(), fullProfileRequest(home)); err != nil {
		t.Fatalf("PreflightHeadlessRun with Linear on second page: %v", err)
	}
}

func registryFixtureCommand(ctx context.Context, _ string, _ ...string) *exec.Cmd {
	return exec.CommandContext(ctx, os.Args[0], "-test.run=^TestCodexAppServerRegistryFixture$")
}

func TestCodexAppServerOperationRegistryRejectsRepeatedCursor(t *testing.T) {
	t.Setenv("CODEX_REGISTRY_FIXTURE_MODE", "repeated-cursor")
	registry := codexAppServerOperationRegistry{
		binary:         "codex",
		commandFactory: registryFixtureCommand,
	}

	surface, err := registry.Operations(context.Background(), codexRuntimeTarget{})
	if err == nil || !strings.Contains(err.Error(), `repeated cursor "same-page"`) {
		t.Fatalf("Operations error = %v, want repeated-cursor error", err)
	}
	if len(surface.Servers) != 0 {
		t.Fatalf("Operations returned partial inventory: %+v", surface.Servers)
	}
}

// TestCodexAppServerOperationRegistryHonoursWorkDir pins the half of BOS-865
// the runtime-target parity ratchet cannot see. Carrying WorkDir on the target
// buys nothing unless the profiled `codex app-server` process actually runs
// there: codex resolves a repo-level `.codex/config.toml` relative to its
// working directory, so a preflight that inherits the daemon's cwd inspects a
// runtime that never loaded the repo's own MCP declaration.
//
// The empty case is equally load-bearing in the other direction — Dir must stay
// empty so exec inherits the daemon cwd exactly as every pre-BOS-865 caller
// (the UNSPECIFIED profile, any non-bossd caller) got.
func TestCodexAppServerOperationRegistryHonoursWorkDir(t *testing.T) {
	fixture, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable: %v", err)
	}
	workDir := t.TempDir()
	tests := []struct {
		name    string
		target  codexRuntimeTarget
		wantDir string
	}{
		{name: "work dir set runs there", target: codexRuntimeTarget{WorkDir: workDir}, wantDir: workDir},
		{name: "work dir unset inherits daemon cwd", target: codexRuntimeTarget{}, wantDir: ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("CODEX_REGISTRY_FIXTURE_MODE", "paginated")
			var captured *exec.Cmd
			registry := codexAppServerOperationRegistry{
				binary: "codex",
				commandFactory: func(ctx context.Context, _ string, _ ...string) *exec.Cmd {
					captured = exec.CommandContext(ctx, fixture, "-test.run=^TestCodexAppServerRegistryFixture$")
					return captured
				},
			}
			if _, err := registry.Operations(context.Background(), tc.target); err != nil {
				t.Fatalf("Operations: %v", err)
			}
			if captured == nil {
				t.Fatal("commandFactory was never called")
			}
			if captured.Dir != tc.wantDir {
				t.Fatalf("cmd.Dir = %q, want %q", captured.Dir, tc.wantDir)
			}
		})
	}
}

// TestCodexAppServerOperationRegistryScopesInventoryToWorkDirThread pins the
// codex behaviour the WorkDir test alone cannot: `mcpServerStatus/list` without
// a thread reports only the global config, because codex applies a repo's
// `.codex/config.toml` per thread cwd. The fixture serves the repo-declared
// Linear server only to a list scoped to a thread started in the work dir.
func TestCodexAppServerOperationRegistryScopesInventoryToWorkDirThread(t *testing.T) {
	workDir := t.TempDir()
	t.Setenv("CODEX_REGISTRY_FIXTURE_MODE", "thread-scoped")
	t.Setenv("CODEX_REGISTRY_FIXTURE_WANT_CWD", workDir)
	registry := codexAppServerOperationRegistry{
		binary:         "codex",
		commandFactory: registryFixtureCommand,
	}

	surface, err := registry.Operations(context.Background(), codexRuntimeTarget{WorkDir: workDir})
	if err != nil {
		t.Fatalf("Operations: %v", err)
	}
	if got := linearRuntimeServers(surface.Servers); len(got) != 1 || got[0].Name != "madverts_linear" {
		t.Fatalf("servers = %+v, want the repo-declared madverts_linear", surface.Servers)
	}
}

func TestCodexAppServerRegistryFixture(t *testing.T) {
	mode := os.Getenv("CODEX_REGISTRY_FIXTURE_MODE")
	if mode == "" {
		return
	}

	decoder := json.NewDecoder(os.Stdin)
	encoder := json.NewEncoder(os.Stdout)
	initialized := mode != "strict-handshake"
	listRequests := 0
	for {
		var request struct {
			ID     *int           `json:"id"`
			Method string         `json:"method"`
			Params map[string]any `json:"params"`
		}
		if err := decoder.Decode(&request); err != nil {
			return
		}
		switch request.Method {
		case "initialize":
			if request.ID == nil {
				t.Fatal("initialize request missing id")
			}
			if err := encoder.Encode(map[string]any{"id": *request.ID, "result": map[string]any{}}); err != nil {
				t.Fatalf("encode initialize response: %v", err)
			}
		case "initialized":
			if request.ID != nil {
				t.Fatal("initialized notification must not include id")
			}
			initialized = true
		case "thread/start":
			if request.ID == nil {
				t.Fatal("thread/start request missing id")
			}
			if want := os.Getenv("CODEX_REGISTRY_FIXTURE_WANT_CWD"); want != "" && request.Params["cwd"] != want {
				t.Fatalf("thread/start cwd = %v, want %q", request.Params["cwd"], want)
			}
			if err := encoder.Encode(map[string]any{
				"id":     *request.ID,
				"result": map[string]any{"thread": map[string]any{"id": "fixture-thread"}},
			}); err != nil {
				t.Fatalf("encode thread/start response: %v", err)
			}
		case "mcpServerStatus/list":
			listRequests++
			if request.ID == nil {
				t.Fatal("registry request missing id")
			}
			if !initialized {
				_ = encoder.Encode(map[string]any{
					"id": *request.ID,
					"error": map[string]any{
						"code":    -32000,
						"message": "initialized notification required",
					},
				})
				return
			}
			switch mode {
			case "repeated-cursor":
				// A third page terminates so a missing cycle guard fails the
				// test without waiting for the probe timeout.
				nextCursor := "same-page"
				if listRequests > 2 {
					nextCursor = ""
				}
				if err := encoder.Encode(map[string]any{
					"id": *request.ID,
					"result": map[string]any{
						"data":       []any{map[string]any{"name": "partial-fixture"}},
						"nextCursor": nextCursor,
					},
				}); err != nil {
					t.Fatalf("encode repeated-cursor response: %v", err)
				}
				if nextCursor == "" {
					return
				}
			case "thread-scoped":
				servers := []any{map[string]any{
					"name":       "codex_apps",
					"authStatus": "bearerToken",
					"tools":      map[string]any{"search": map[string]any{"name": "search"}},
				}}
				if request.Params["threadId"] == "fixture-thread" {
					tools := map[string]any{}
					for _, operation := range realLinearConnectorOperations() {
						tools[operation] = map[string]any{"name": operation}
					}
					servers = append(servers, map[string]any{
						"name":       "madverts_linear",
						"authStatus": "bearerToken",
						"tools":      tools,
					})
				}
				_ = encoder.Encode(map[string]any{"id": *request.ID, "result": map[string]any{"data": servers}})
				return
			case "large-response":
				_ = encoder.Encode(map[string]any{
					"id": *request.ID,
					"result": map[string]any{
						"data": []any{map[string]any{
							"name":       "large-fixture",
							"authStatus": "oAuth",
							"tools":      map[string]any{"read_issue": map[string]any{"name": "read_issue"}},
						}},
						"padding": strings.Repeat("x", 70*1024),
					},
				})
				return
			case "paginated":
				cursor, _ := request.Params["cursor"].(string)
				if cursor == "" {
					_ = encoder.Encode(map[string]any{
						"id": *request.ID,
						"result": map[string]any{
							"data": []any{map[string]any{
								"name":       "github",
								"authStatus": "oAuth",
								"tools":      map[string]any{"read_file": map[string]any{"name": "read_file"}},
							}},
							"nextCursor": "linear-page",
						},
					})
					continue
				}
				if cursor != "linear-page" {
					t.Fatalf("unexpected cursor %q", cursor)
				}
				tools := map[string]any{}
				for _, operation := range realLinearConnectorOperations() {
					tools[operation] = map[string]any{"name": operation}
				}
				_ = encoder.Encode(map[string]any{
					"id": *request.ID,
					"result": map[string]any{
						"data": []any{map[string]any{
							"name":       "linear@openai-curated",
							"authStatus": "oAuth",
							"tools":      tools,
						}},
					},
				})
				return
			default:
				_ = encoder.Encode(map[string]any{
					"id":     *request.ID,
					"result": map[string]any{"data": []any{}},
				})
				return
			}
		default:
			t.Fatalf("unexpected method %q", request.Method)
		}
	}
}

func TestCodexOperationRegistryProbeBudget(t *testing.T) {
	for _, shortCaller := range []bool{false, true} {
		name := "probe ceiling"
		if shortCaller {
			name = "shorter caller"
		}
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			var callerDeadline time.Time
			if shortCaller {
				callerDeadline = time.Now().Add(time.Second)
				var cancel context.CancelFunc
				ctx, cancel = context.WithDeadline(ctx, callerDeadline)
				defer cancel()
			}
			var deadline time.Time
			registry := codexAppServerOperationRegistry{commandFactory: func(ctx context.Context, _ string, _ ...string) *exec.Cmd {
				deadline, _ = ctx.Deadline()
				return exec.CommandContext(ctx, "/nonexistent-codex-budget-fixture")
			}}
			before := time.Now()
			_, err := registry.Operations(ctx, codexRuntimeTarget{})
			after := time.Now()
			if err == nil {
				t.Fatal("nonexistent command succeeded")
			}
			if shortCaller {
				if !deadline.Equal(callerDeadline) {
					t.Fatalf("deadline = %v, want caller %v", deadline, callerDeadline)
				}
			} else if deadline.Before(before.Add(sharedplugin.HeadlessCapabilityProbeTimeout)) || deadline.After(after.Add(sharedplugin.HeadlessCapabilityProbeTimeout)) {
				t.Fatalf("deadline = %v, want shared probe budget", deadline)
			}
		})
	}
}
