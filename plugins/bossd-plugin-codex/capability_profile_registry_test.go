package main

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"strings"
	"testing"
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

func TestCodexAppServerRegistryFixture(t *testing.T) {
	mode := os.Getenv("CODEX_REGISTRY_FIXTURE_MODE")
	if mode == "" {
		return
	}

	decoder := json.NewDecoder(os.Stdin)
	encoder := json.NewEncoder(os.Stdout)
	initialized := mode != "strict-handshake"
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
		case "mcpServerStatus/list":
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
