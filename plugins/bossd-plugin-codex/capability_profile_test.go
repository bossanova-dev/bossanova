package main

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/rs/zerolog"
	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	bossanovav1 "github.com/recurser/bossalib/gen/bossanova/v1"
)

func TestPreflightHeadlessRunRejectsRestrictedRuntimeWithoutStartingRunner(t *testing.T) {
	var actualRunnerStarted atomic.Bool
	home := t.TempDir()
	srv := newServer(nil, zerolog.Nop(), WithCommandFactory(func(context.Context, string, ...string) *exec.Cmd {
		actualRunnerStarted.Store(true)
		return nil
	}))
	srv.operationRegistry = runtimeOperationRegistryFunc(func(_ context.Context, target codexRuntimeTarget) (runtimeOperationSurface, error) {
		if target.Home != home || target.ExtraEnv["CODEX_HOME"] != home {
			t.Fatalf("preflight home = target=%q env=%q, want %q", target.Home, target.ExtraEnv["CODEX_HOME"], home)
		}
		if target.Model != "gpt-5.6" {
			t.Fatalf("preflight model = %q, want gpt-5.6", target.Model)
		}
		return runtimeOperationSurface{
			Source: codexOperationRegistrySource,
			Servers: []runtimeMCPServer{{
				Name:       "linear@openai-curated",
				AuthStatus: "oAuth",
				Operations: []string{"read_issue", "list_attachments", "download_attachment"},
			}},
		}, nil
	})

	_, err := srv.PreflightHeadlessRun(context.Background(), &bossanovav1.PreflightHeadlessRunRequest{
		Model:                     "gpt-5.6",
		ExtraEnv:                  map[string]string{"CODEX_HOME": home},
		HeadlessCapabilityProfile: bossanovav1.HeadlessCapabilityProfile_HEADLESS_CAPABILITY_PROFILE_TRACKER_PLAN_ATTACHMENT_V1,
	})
	if status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("PreflightHeadlessRun code = %s, want FailedPrecondition; err=%v", status.Code(err), err)
	}
	if actualRunnerStarted.Load() {
		t.Fatal("preflight must not invoke the actual runner")
	}
}

func TestPreflightHeadlessRunReturnsRuntimeOperationRegistrySurface(t *testing.T) {
	home := t.TempDir()
	srv := newTestServer(t)
	srv.operationRegistry = runtimeOperationRegistryFunc(func(_ context.Context, target codexRuntimeTarget) (runtimeOperationSurface, error) {
		if target.Home != home || target.ExtraEnv["CODEX_HOME"] != home {
			t.Fatalf("preflight home = target=%q env=%q, want %q", target.Home, target.ExtraEnv["CODEX_HOME"], home)
		}
		if target.Model != "gpt-5.6" {
			t.Fatalf("preflight model = %q, want gpt-5.6", target.Model)
		}
		return runtimeOperationSurface{
			Source: codexOperationRegistrySource,
			Servers: []runtimeMCPServer{{
				Name:       "linear@openai-curated",
				AuthStatus: "oAuth",
				Operations: []string{
					"read_issue",
					"list_attachments",
					"download_attachment",
					"prepare_attachment_upload",
					"upload_attachment",
					"finalize_attachment_upload",
					"update_issue",
				},
			}},
		}, nil
	})

	resp, err := srv.PreflightHeadlessRun(context.Background(), fullProfileRequest(home))
	if err != nil {
		t.Fatalf("PreflightHeadlessRun: %v", err)
	}
	if resp.GetSource() != codexOperationRegistrySource {
		t.Fatalf("Source = %q, want %q", resp.GetSource(), codexOperationRegistrySource)
	}
}

func fullProfileRequest(home string) *bossanovav1.PreflightHeadlessRunRequest {
	return &bossanovav1.PreflightHeadlessRunRequest{
		Model:                     "gpt-5.6",
		ExtraEnv:                  map[string]string{"CODEX_HOME": home},
		HeadlessCapabilityProfile: bossanovav1.HeadlessCapabilityProfile_HEADLESS_CAPABILITY_PROFILE_TRACKER_PLAN_ATTACHMENT_V1,
	}
}

func TestStartRunTrackerPlanAttachmentRejectsRestrictedRuntimeBeforeRunnerStart(t *testing.T) {
	var started atomic.Bool
	home := t.TempDir()
	srv := newServer(nil, zerolog.Nop(), WithCommandFactory(func(context.Context, string, ...string) *exec.Cmd {
		started.Store(true)
		return nil
	}))
	srv.operationRegistry = runtimeOperationRegistryFunc(func(context.Context, codexRuntimeTarget) (runtimeOperationSurface, error) {
		return runtimeOperationSurface{
			Source: "codex app-server mcpServerStatus/list",
			Servers: []runtimeMCPServer{{
				Name:       "linear@openai-curated",
				AuthStatus: "oAuth",
				Operations: []string{"read_issue", "list_attachments", "download_attachment"},
			}},
		}, nil
	})

	_, err := srv.StartRun(context.Background(), &bossanovav1.StartAgentRunRequest{
		WorkDir:                   t.TempDir(),
		Plan:                      "actual work prompt must not start",
		SessionId:                 "capability-reduced",
		LogPath:                   t.TempDir() + "/codex.jsonl",
		ExtraEnv:                  map[string]string{"CODEX_HOME": home},
		HeadlessCapabilityProfile: bossanovav1.HeadlessCapabilityProfile_HEADLESS_CAPABILITY_PROFILE_TRACKER_PLAN_ATTACHMENT_V1,
	})
	if status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("StartRun code = %s, want FailedPrecondition; err=%v", status.Code(err), err)
	}
	if started.Load() {
		t.Fatal("reduced runtime must not invoke the actual runner")
	}
	for _, want := range []string{"plan_publication", "ticket_mutation"} {
		if !strings.Contains(status.Convert(err).Message(), want) {
			t.Errorf("diagnosis %q missing %q", status.Convert(err).Message(), want)
		}
	}
	var diagnostic *errdetails.ErrorInfo
	for _, detail := range status.Convert(err).Details() {
		if info, ok := detail.(*errdetails.ErrorInfo); ok {
			diagnostic = info
			break
		}
	}
	if diagnostic == nil {
		t.Fatal("FailedPrecondition must include structured ErrorInfo diagnostics")
	}
	if !strings.Contains(diagnostic.GetMetadata()["provided"], "list_attachments") ||
		!strings.Contains(diagnostic.GetMetadata()["source"], "connector_restricted_to_list_read_operations") {
		t.Fatalf("unsafe or incomplete diagnostic metadata: %v", diagnostic.GetMetadata())
	}
}

func TestStartRunTrackerPlanAttachmentStartsOnceWithProjectedHomeAndModel(t *testing.T) {
	var starts atomic.Int32
	home := t.TempDir()
	srv := newServer(nil, zerolog.Nop(), WithCommandFactory(func(ctx context.Context, _ string, _ ...string) *exec.Cmd {
		starts.Add(1)
		return exec.CommandContext(ctx, "/bin/sh", "-c", `printf '%s\n' '{"type":"thread.started","thread_id":"profiled-thread"}'`)
	}))
	srv.operationRegistry = runtimeOperationRegistryFunc(func(_ context.Context, target codexRuntimeTarget) (runtimeOperationSurface, error) {
		if target.Home != home || target.ExtraEnv["CODEX_HOME"] != home {
			t.Fatalf("preflight home = target=%q env=%q, want %q", target.Home, target.ExtraEnv["CODEX_HOME"], home)
		}
		if target.Model != "gpt-profiled" {
			t.Fatalf("preflight model = %q, want gpt-profiled", target.Model)
		}
		return runtimeOperationSurface{Source: "codex app-server mcpServerStatus/list", Servers: []runtimeMCPServer{{
			Name:       "linear@openai-curated",
			AuthStatus: "oAuth",
			Operations: []string{"read_issue", "list_attachments", "download_attachment", "prepare_attachment_upload", "upload_attachment", "finalize_attachment_upload", "update_issue"},
		}}}, nil
	})

	resp, err := srv.StartRun(context.Background(), &bossanovav1.StartAgentRunRequest{
		WorkDir:                   t.TempDir(),
		Plan:                      "actual work prompt",
		SessionId:                 "capability-full",
		LogPath:                   t.TempDir() + "/codex.jsonl",
		Model:                     "gpt-profiled",
		ExtraEnv:                  map[string]string{"CODEX_HOME": home},
		HeadlessCapabilityProfile: bossanovav1.HeadlessCapabilityProfile_HEADLESS_CAPABILITY_PROFILE_TRACKER_PLAN_ATTACHMENT_V1,
	})
	if err != nil {
		t.Fatalf("StartRun: %v", err)
	}
	if resp.GetSessionId() != "profiled-thread" {
		t.Fatalf("SessionId = %q, want profiled-thread", resp.GetSessionId())
	}
	if got := starts.Load(); got != 1 {
		t.Fatalf("actual runner starts = %d, want 1", got)
	}
}

func TestTrackerPlanAttachmentRequiresOneAuthenticatedLinearServer(t *testing.T) {
	home := t.TempDir()
	srv := newTestServer(t)
	srv.operationRegistry = runtimeOperationRegistryFunc(func(context.Context, codexRuntimeTarget) (runtimeOperationSurface, error) {
		return runtimeOperationSurface{Source: codexOperationRegistrySource, Servers: []runtimeMCPServer{
			{Name: "linear-a", AuthStatus: "oAuth", Operations: []string{"read_issue"}},
			{Name: "linear-b", AuthStatus: "oAuth", Operations: []string{"list_attachments", "download_attachment", "prepare_attachment_upload", "upload_attachment", "finalize_attachment_upload", "update_issue"}},
		}}, nil
	})

	_, err := srv.preflightHeadlessCapabilityProfile(
		context.Background(),
		bossanovav1.HeadlessCapabilityProfile_HEADLESS_CAPABILITY_PROFILE_TRACKER_PLAN_ATTACHMENT_V1,
		codexRuntimeTarget{
			Home:     home,
			Model:    "gpt-profiled",
			ExtraEnv: map[string]string{"CODEX_HOME": home},
		},
	)
	if status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("split Linear surfaces must fail closed, got %v", err)
	}
}

func TestTrackerPlanAttachmentRejectsSimilarlyNamedUnrelatedOperations(t *testing.T) {
	home := t.TempDir()
	srv := newTestServer(t)
	srv.operationRegistry = runtimeOperationRegistryFunc(func(context.Context, codexRuntimeTarget) (runtimeOperationSurface, error) {
		return runtimeOperationSurface{Source: codexOperationRegistrySource, Servers: []runtimeMCPServer{{
			Name:       "linear@openai-curated",
			AuthStatus: "oAuth",
			Operations: []string{"read_issue_summary", "list_attachment_revisions", "download_attachment_preview", "prepare_attachment_upload_preview", "finalize_attachment_preview", "update_issue_view"},
		}}}, nil
	})

	_, err := srv.preflightHeadlessCapabilityProfile(
		context.Background(),
		bossanovav1.HeadlessCapabilityProfile_HEADLESS_CAPABILITY_PROFILE_TRACKER_PLAN_ATTACHMENT_V1,
		codexRuntimeTarget{
			Home:     home,
			ExtraEnv: map[string]string{"CODEX_HOME": home},
		},
	)
	if status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("similarly named unrelated tools must fail closed, got %v", err)
	}
}

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
				for _, operation := range []string{
					"read_issue",
					"list_attachments",
					"download_attachment",
					"prepare_attachment_upload",
					"upload_attachment",
					"finalize_attachment_upload",
					"update_issue",
				} {
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
