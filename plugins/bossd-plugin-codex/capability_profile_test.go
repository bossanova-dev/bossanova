package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/rs/zerolog"
	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	bossanovav1 "github.com/recurser/bossalib/gen/bossanova/v1"
)

// realLinearConnectorOperations is the tool surface the Linear connector
// actually exposes for the plan-attachment workflow — the names
// buildLinearOperationMap in skills-toolbox/tracker/linear.mjs declares — plus
// one unrelated tool as noise. Fixtures build on this so a preflight
// happy-path is never proven against a connector that cannot exist.
func realLinearConnectorOperations() []string {
	return []string{
		"get_issue",
		"get_attachment",
		"prepare_attachment_upload",
		"create_attachment_from_upload",
		"save_issue",
		"list_issues",
	}
}

// readOnlyLinearConnectorOperations is the real connector surface with every
// write tool withheld, modelling a read-restricted connector.
func readOnlyLinearConnectorOperations() []string {
	return []string{"get_issue", "get_attachment", "list_issues"}
}

// realLinearConnectorOperationsWithout returns the real surface minus one tool,
// modelling a runtime that is under-provisioned in exactly one way.
func realLinearConnectorOperationsWithout(omit string) []string {
	kept := make([]string, 0, len(realLinearConnectorOperations()))
	for _, operation := range realLinearConnectorOperations() {
		if operation != omit {
			kept = append(kept, operation)
		}
	}
	return kept
}

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
				Operations: readOnlyLinearConnectorOperations(),
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
				Operations: realLinearConnectorOperations(),
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

// TestPreflightHeadlessRunUsesAmbientCodexHome keeps account-0/unmanaged
// launches on the same Codex home the real subprocess inherits. The account
// overlay intentionally has no CODEX_HOME, so the preflight must use the
// daemon's ambient home without projecting a new variable into the request.
func TestPreflightHeadlessRunUsesAmbientCodexHome(t *testing.T) {
	home := t.TempDir()
	t.Setenv("CODEX_HOME", home)
	srv := newTestServer(t)
	srv.operationRegistry = runtimeOperationRegistryFunc(func(_ context.Context, target codexRuntimeTarget) (runtimeOperationSurface, error) {
		if target.Home != home {
			t.Fatalf("preflight home = %q, want ambient %q", target.Home, home)
		}
		if _, projected := target.ExtraEnv["CODEX_HOME"]; projected {
			t.Fatalf("preflight must not project CODEX_HOME into unmanaged env: %v", target.ExtraEnv)
		}
		return runtimeOperationSurface{Source: codexOperationRegistrySource, Servers: []runtimeMCPServer{{
			Name:       "linear@openai-curated",
			AuthStatus: "oAuth",
			Operations: realLinearConnectorOperations(),
		}}}, nil
	})

	if _, err := srv.PreflightHeadlessRun(context.Background(), &bossanovav1.PreflightHeadlessRunRequest{
		HeadlessCapabilityProfile: bossanovav1.HeadlessCapabilityProfile_HEADLESS_CAPABILITY_PROFILE_TRACKER_PLAN_ATTACHMENT_V1,
	}); err != nil {
		t.Fatalf("PreflightHeadlessRun: %v", err)
	}
}

func TestPreflightHeadlessRunUsesDefaultCodexHomeWhenAmbientHomeIsUnset(t *testing.T) {
	home := t.TempDir()
	codexHome := filepath.Join(home, ".codex")
	if err := os.Mkdir(codexHome, 0o700); err != nil {
		t.Fatalf("create default codex home: %v", err)
	}
	t.Setenv("HOME", home)
	t.Setenv("CODEX_HOME", "")
	srv := newTestServer(t)
	srv.operationRegistry = runtimeOperationRegistryFunc(func(_ context.Context, target codexRuntimeTarget) (runtimeOperationSurface, error) {
		if target.Home != codexHome {
			t.Fatalf("preflight home = %q, want default %q", target.Home, codexHome)
		}
		return runtimeOperationSurface{Source: codexOperationRegistrySource, Servers: []runtimeMCPServer{{
			Name:       "linear@openai-curated",
			AuthStatus: "oAuth",
			Operations: realLinearConnectorOperations(),
		}}}, nil
	})

	if _, err := srv.PreflightHeadlessRun(context.Background(), &bossanovav1.PreflightHeadlessRunRequest{
		HeadlessCapabilityProfile: bossanovav1.HeadlessCapabilityProfile_HEADLESS_CAPABILITY_PROFILE_TRACKER_PLAN_ATTACHMENT_V1,
	}); err != nil {
		t.Fatalf("PreflightHeadlessRun: %v", err)
	}
}

// TestPreflightHeadlessRunResolvesConfiguredRunnerModel pins that the
// pre-worktree preflight profiles the runtime StartRun will actually launch: an
// empty request model must fall back to the plugin-configured default
// (BOSS_PLUGIN_model), because the registry passes the model to
// `codex app-server` as `-c model="…"` and would otherwise enumerate a
// different operation surface than the real run.
func TestPreflightHeadlessRunResolvesConfiguredRunnerModel(t *testing.T) {
	home := t.TempDir()
	srv := newTestServer(t, WithModel("codex-default"))
	var gotModel string
	srv.operationRegistry = runtimeOperationRegistryFunc(func(_ context.Context, target codexRuntimeTarget) (runtimeOperationSurface, error) {
		gotModel = target.Model
		return runtimeOperationSurface{}, nil
	})

	_, _ = srv.PreflightHeadlessRun(context.Background(), &bossanovav1.PreflightHeadlessRunRequest{
		ExtraEnv:                  map[string]string{"CODEX_HOME": home},
		HeadlessCapabilityProfile: bossanovav1.HeadlessCapabilityProfile_HEADLESS_CAPABILITY_PROFILE_TRACKER_PLAN_ATTACHMENT_V1,
	})
	if gotModel != "codex-default" {
		t.Fatalf("preflight model = %q, want codex-default (configured runner default)", gotModel)
	}
}

// TestPreflightHeadlessRunRequestModelOverridesRunnerModel pins the other half
// of resolveCodexModel's rule on the preflight path: an explicit request model
// still wins over the configured default.
func TestPreflightHeadlessRunRequestModelOverridesRunnerModel(t *testing.T) {
	home := t.TempDir()
	srv := newTestServer(t, WithModel("codex-default"))
	var gotModel string
	srv.operationRegistry = runtimeOperationRegistryFunc(func(_ context.Context, target codexRuntimeTarget) (runtimeOperationSurface, error) {
		gotModel = target.Model
		return runtimeOperationSurface{}, nil
	})

	_, _ = srv.PreflightHeadlessRun(context.Background(), &bossanovav1.PreflightHeadlessRunRequest{
		Model:                     "gpt-requested",
		ExtraEnv:                  map[string]string{"CODEX_HOME": home},
		HeadlessCapabilityProfile: bossanovav1.HeadlessCapabilityProfile_HEADLESS_CAPABILITY_PROFILE_TRACKER_PLAN_ATTACHMENT_V1,
	})
	if gotModel != "gpt-requested" {
		t.Fatalf("preflight model = %q, want gpt-requested (request wins)", gotModel)
	}
}

// TestPreflightAndStartRunInspectIdenticalRuntimeTarget is the drift guard: the
// two paths must build their codexRuntimeTarget through the same helper, so a
// field added to the target can never be populated on only one of them.
func TestPreflightAndStartRunInspectIdenticalRuntimeTarget(t *testing.T) {
	home := t.TempDir()
	// Two distinct map instances holding identical content: handing both RPCs
	// the *same* map would make the reflect.DeepEqual below compare one map
	// against itself, blind to a future path that injects into its own ExtraEnv.
	preflightEnv := map[string]string{"CODEX_HOME": home, "CODEX_EXTRA": "shared"}
	startEnv := map[string]string{"CODEX_HOME": home, "CODEX_EXTRA": "shared"}
	// One work dir sent on BOTH requests. Sending it only on StartRun (as this
	// test did before WorkDir joined codexRuntimeTarget) is exactly the BOS-865
	// divergence, and the DeepEqual below is what refuses it.
	workDir := t.TempDir()
	// Guard against a real `codex` subprocess: if the profile check ever stops
	// rejecting, StartRun would otherwise fall through to runner.Start with the
	// production command factory and leak a detached process out of this test.
	var runnerStarted atomic.Bool
	srv := newTestServer(t, WithModel("codex-default"), WithCommandFactory(func(context.Context, string, ...string) *exec.Cmd {
		runnerStarted.Store(true)
		return nil
	}))
	var targets []codexRuntimeTarget
	srv.operationRegistry = runtimeOperationRegistryFunc(func(_ context.Context, target codexRuntimeTarget) (runtimeOperationSurface, error) {
		targets = append(targets, target)
		// Fail closed so neither path proceeds past the profile check; the
		// captured targets are what this test compares.
		return runtimeOperationSurface{}, errors.New("registry unavailable")
	})

	if _, err := srv.PreflightHeadlessRun(context.Background(), &bossanovav1.PreflightHeadlessRunRequest{
		ExtraEnv:                  preflightEnv,
		WorkDir:                   workDir,
		HeadlessCapabilityProfile: bossanovav1.HeadlessCapabilityProfile_HEADLESS_CAPABILITY_PROFILE_TRACKER_PLAN_ATTACHMENT_V1,
	}); status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("PreflightHeadlessRun code = %s, want FailedPrecondition", status.Code(err))
	}
	if _, err := srv.StartRun(context.Background(), &bossanovav1.StartAgentRunRequest{
		WorkDir:                   workDir,
		Plan:                      "must not start",
		SessionId:                 "drift-guard",
		LogPath:                   t.TempDir() + "/codex.jsonl",
		ExtraEnv:                  startEnv,
		HeadlessCapabilityProfile: bossanovav1.HeadlessCapabilityProfile_HEADLESS_CAPABILITY_PROFILE_TRACKER_PLAN_ATTACHMENT_V1,
	}); status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("StartRun code = %s, want FailedPrecondition", status.Code(err))
	}
	if runnerStarted.Load() {
		t.Fatal("StartRun spawned the codex runner; the profile check must reject before any run side effect")
	}

	if len(targets) != 2 {
		t.Fatalf("registry calls = %d, want 2", len(targets))
	}
	if !reflect.DeepEqual(targets[0], targets[1]) {
		t.Fatalf("preflight target %+v != StartRun target %+v", targets[0], targets[1])
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
				Operations: readOnlyLinearConnectorOperations(),
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
	if !strings.Contains(diagnostic.GetMetadata()["provided"], "ticket_read.named_issue") ||
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
			Operations: realLinearConnectorOperations(),
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

// errorInfoFrom extracts the structured diagnostic a capability failure must
// always carry, failing the test when it is absent.
func errorInfoFrom(t *testing.T, err error) *errdetails.ErrorInfo {
	t.Helper()
	for _, detail := range status.Convert(err).Details() {
		if info, ok := detail.(*errdetails.ErrorInfo); ok {
			return info
		}
	}
	t.Fatal("FailedPrecondition must include structured ErrorInfo diagnostics")
	return nil
}

// preflightWithServers runs the profiled preflight against a fixed runtime
// surface, returning the error for message/diagnostic assertions.
func preflightWithServers(t *testing.T, target codexRuntimeTarget, servers []runtimeMCPServer) error {
	t.Helper()
	home := t.TempDir()
	srv := newTestServer(t)
	srv.operationRegistry = runtimeOperationRegistryFunc(func(context.Context, codexRuntimeTarget) (runtimeOperationSurface, error) {
		return runtimeOperationSurface{Source: codexOperationRegistrySource, Servers: servers}, nil
	})
	target.Home = home
	target.ExtraEnv = map[string]string{"CODEX_HOME": home}
	_, err := srv.preflightHeadlessCapabilityProfile(
		context.Background(),
		bossanovav1.HeadlessCapabilityProfile_HEADLESS_CAPABILITY_PROFILE_TRACKER_PLAN_ATTACHMENT_V1,
		target,
	)
	return err
}

// TestTrackerPlanAttachmentClassifiesDeclaredButEmptyConnector is the BOS-865
// classification. Codex reports authStatus=bearerToken with ZERO tools when the
// server's bearer_token_env_var is present-but-empty, which is exactly what
// dotenv.OverlayWithRepo guarantees when the key is configured nowhere. Feeding
// that to the requirement matrix produced the full seven-name Missing list and
// told the operator to fix a declaration that was already correct.
func TestTrackerPlanAttachmentClassifiesDeclaredButEmptyConnector(t *testing.T) {
	err := preflightWithServers(t, codexRuntimeTarget{WorkDir: t.TempDir()}, []runtimeMCPServer{
		{Name: "bossanova-linear", AuthStatus: "bearerToken", Operations: nil},
	})
	if status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("code = %s, want FailedPrecondition; err=%v", status.Code(err), err)
	}
	message := status.Convert(err).Message()
	// The source marker must reach the gRPC message, not only ErrorInfo
	// metadata: a `boss` operator sees the message and nothing else.
	if !strings.Contains(message, "connector_declared_but_exposes_no_operations") {
		t.Errorf("message %q must carry the connector_declared_but_exposes_no_operations marker", message)
	}
	// The message must not send the operator to fix a correct declaration.
	if !strings.Contains(message, "the declaration is present and correct") {
		t.Errorf("message %q must state the declaration is correct", message)
	}
	// Both plausible causes named, neither asserted: an unreachable server at
	// load looks identical to an empty credential.
	for _, want := range []string{"bearer-token env var", "reachable at load"} {
		if !strings.Contains(message, want) {
			t.Errorf("message %q must mention %q as a candidate cause", message, want)
		}
	}
	// The connector and its auth status are named; the credential never is.
	if !strings.Contains(message, `"bossanova-linear"`) || !strings.Contains(message, `"bearerToken"`) {
		t.Errorf("message %q must name the server and its auth status", message)
	}
	if !strings.HasPrefix(message, "tracker-plan-attachment unavailable") {
		t.Errorf("message %q lost the load-bearing prefix", message)
	}
	diagnostic := errorInfoFrom(t, err)
	if diagnostic.GetReason() != "TRACKER_PLAN_ATTACHMENT_UNAVAILABLE" {
		t.Errorf("ErrorInfo.Reason = %q, want TRACKER_PLAN_ATTACHMENT_UNAVAILABLE", diagnostic.GetReason())
	}
	// Missing must stay populated for machine consumers: the profile genuinely
	// is unmet, so a consumer still needs to know which requirements went
	// unsatisfied. The zero-operations branch calls failedCapabilityPrecondition
	// directly, so it fails closed whatever Missing holds — this asserts the
	// diagnostic stays useful, not that it is what keeps the gate shut.
	var missing []string
	if err := json.Unmarshal([]byte(diagnostic.GetMetadata()["missing"]), &missing); err != nil {
		t.Fatalf("decode missing metadata: %v", err)
	}
	if !slices.Equal(missing, uniqueSorted(trackerPlanAttachmentRequirements())) {
		t.Errorf("missing = %q, want the full requirement set", missing)
	}
}

// TestTrackerPlanAttachmentUndeclaredConnectorStillFailsClosed is the AC2 pin
// for candidate direction 4: a repo that genuinely has not declared the server
// must keep failing with its existing marker and the full seven-name list. This
// is the case the gate exists for, and the new classification must not swallow
// it.
func TestTrackerPlanAttachmentUndeclaredConnectorStillFailsClosed(t *testing.T) {
	err := preflightWithServers(t, codexRuntimeTarget{WorkDir: t.TempDir()}, []runtimeMCPServer{
		{Name: "some-other-connector", AuthStatus: "oAuth", Operations: []string{"unrelated_tool"}},
	})
	if status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("code = %s, want FailedPrecondition; err=%v", status.Code(err), err)
	}
	message := status.Convert(err).Message()
	if !strings.Contains(message, "connector_plugin_disabled_or_missing") {
		t.Errorf("message %q must carry connector_plugin_disabled_or_missing", message)
	}
	if strings.Contains(message, "connector_declared_but_exposes_no_operations") {
		t.Errorf("message %q must not reclassify an undeclared connector as a credential problem", message)
	}
	for _, requirement := range trackerPlanAttachmentRequirements() {
		if !strings.Contains(message, requirement) {
			t.Errorf("message %q missing requirement %q", message, requirement)
		}
	}
}

// TestTrackerPlanAttachmentPartialSurfaceStillNamesMissingOperations guards the
// other edge of the classification: a connector exposing a real but incomplete
// surface is a genuine capability gap and must keep naming the specific
// operations, not be absorbed into the credential branch.
func TestTrackerPlanAttachmentPartialSurfaceStillNamesMissingOperations(t *testing.T) {
	err := preflightWithServers(t, codexRuntimeTarget{WorkDir: t.TempDir()}, []runtimeMCPServer{
		{Name: "bossanova-linear", AuthStatus: "bearerToken", Operations: []string{"get_issue", "save_issue"}},
	})
	if status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("code = %s, want FailedPrecondition; err=%v", status.Code(err), err)
	}
	message := status.Convert(err).Message()
	if strings.Contains(message, "connector_declared_but_exposes_no_operations") {
		t.Fatalf("message %q reclassified a real capability gap as a credential problem", message)
	}
	// The three save_issue-backed requirements and get_issue are satisfied; the
	// attachment operations are the real gap and must be named.
	for _, want := range []string{
		"plan_retrieval.download_attachment",
		"plan_publication.prepare_markdown_attachment",
		"plan_publication.finalize_markdown_attachment",
	} {
		if !strings.Contains(message, want) {
			t.Errorf("message %q must name missing requirement %q", message, want)
		}
	}
	for _, unwanted := range []string{"ticket_read.named_issue", "ticket_mutation.update_issue_fields"} {
		if strings.Contains(message, unwanted) {
			t.Errorf("message %q names %q as missing, but it is provided", message, unwanted)
		}
	}
}

// TestTrackerPlanAttachmentMarksUnsetWorkDir pins the self-diagnosing half of
// the work-dir fix: a profiled check whose caller never plumbed a work dir
// cannot have loaded the repo's own .codex/config.toml, so it says so rather
// than silently regressing to the manufactured capability gap of BOS-865.
func TestTrackerPlanAttachmentMarksUnsetWorkDir(t *testing.T) {
	servers := []runtimeMCPServer{{Name: "some-other-connector", AuthStatus: "oAuth", Operations: []string{"unrelated_tool"}}}

	unset := status.Convert(preflightWithServers(t, codexRuntimeTarget{}, servers)).Message()
	if !strings.Contains(unset, "preflight_work_dir_unset") {
		t.Errorf("message %q must carry preflight_work_dir_unset when no work dir was supplied", unset)
	}
	set := status.Convert(preflightWithServers(t, codexRuntimeTarget{WorkDir: t.TempDir()}, servers)).Message()
	if strings.Contains(set, "preflight_work_dir_unset") {
		t.Errorf("message %q must not carry preflight_work_dir_unset when a work dir was supplied", set)
	}
}

func TestTrackerPlanAttachmentRequiresOneAuthenticatedLinearServer(t *testing.T) {
	home := t.TempDir()
	srv := newTestServer(t)
	srv.operationRegistry = runtimeOperationRegistryFunc(func(context.Context, codexRuntimeTarget) (runtimeOperationSurface, error) {
		return runtimeOperationSurface{Source: codexOperationRegistrySource, Servers: []runtimeMCPServer{
			{Name: "linear-a", AuthStatus: "oAuth", Operations: []string{"get_issue"}},
			{Name: "linear-b", AuthStatus: "oAuth", Operations: []string{"get_attachment", "prepare_attachment_upload", "create_attachment_from_upload", "save_issue"}},
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

func TestTrackerPlanAttachmentRequirementsExactCanonicalSet(t *testing.T) {
	want := []string{
		"ticket_read.named_issue",
		"plan_retrieval.download_attachment",
		"plan_publication.prepare_markdown_attachment",
		"plan_publication.finalize_markdown_attachment",
		"ticket_mutation.update_issue_fields",
		"ticket_mutation.update_issue_state",
		"ticket_mutation.update_issue_metadata",
	}
	if got := trackerPlanAttachmentRequirements(); !slices.Equal(got, want) {
		t.Fatalf("trackerPlanAttachmentRequirements() = %q, want %q", got, want)
	}
}

func TestTrackerPlanAttachmentMatrixAndPreflightAcceptRealLinearConnectorSurface(t *testing.T) {
	operations := realLinearConnectorOperations()

	t.Run("matrix", func(t *testing.T) {
		provided, missing := trackerPlanAttachmentMatrix(operations)
		if len(missing) != 0 {
			t.Fatalf("missing = %q, want none", missing)
		}
		if want := trackerPlanAttachmentRequirements(); !slices.Equal(provided, want) {
			t.Fatalf("provided = %q, want %q", provided, want)
		}
	})

	t.Run("preflight", func(t *testing.T) {
		home := t.TempDir()
		srv := newTestServer(t)
		srv.operationRegistry = runtimeOperationRegistryFunc(func(context.Context, codexRuntimeTarget) (runtimeOperationSurface, error) {
			return runtimeOperationSurface{Source: codexOperationRegistrySource, Servers: []runtimeMCPServer{{
				Name:       "linear@openai-curated",
				AuthStatus: "oAuth",
				Operations: operations,
			}}}, nil
		})

		if _, err := srv.preflightHeadlessCapabilityProfile(
			context.Background(),
			bossanovav1.HeadlessCapabilityProfile_HEADLESS_CAPABILITY_PROFILE_TRACKER_PLAN_ATTACHMENT_V1,
			codexRuntimeTarget{
				Home:     home,
				ExtraEnv: map[string]string{"CODEX_HOME": home},
			},
		); err != nil {
			t.Fatalf("preflightHeadlessCapabilityProfile: %v, want nil", err)
		}
	})
}

// TestTrackerPlanAttachmentFailsClosedWhenAnyRequiredToolIsMissing drops each
// required connector tool from the real surface in turn, so every one of the
// seven requirements has fail-closed coverage rather than only the
// get_attachment case. wantMissing is the sorted requirement-name set, matching
// the uniqueSorted diagnostic payload.
func TestTrackerPlanAttachmentFailsClosedWhenAnyRequiredToolIsMissing(t *testing.T) {
	for _, testCase := range []struct {
		omit        string
		wantMissing []string
	}{
		{omit: "get_issue", wantMissing: []string{"ticket_read.named_issue"}},
		{omit: "get_attachment", wantMissing: []string{"plan_retrieval.download_attachment"}},
		{omit: "prepare_attachment_upload", wantMissing: []string{"plan_publication.prepare_markdown_attachment"}},
		{omit: "create_attachment_from_upload", wantMissing: []string{"plan_publication.finalize_markdown_attachment"}},
		{omit: "save_issue", wantMissing: []string{
			"ticket_mutation.update_issue_fields",
			"ticket_mutation.update_issue_metadata",
			"ticket_mutation.update_issue_state",
		}},
	} {
		t.Run(testCase.omit, func(t *testing.T) {
			home := t.TempDir()
			srv := newTestServer(t)
			srv.operationRegistry = runtimeOperationRegistryFunc(func(context.Context, codexRuntimeTarget) (runtimeOperationSurface, error) {
				return runtimeOperationSurface{Source: codexOperationRegistrySource, Servers: []runtimeMCPServer{{
					Name:       "linear@openai-curated",
					AuthStatus: "oAuth",
					Operations: realLinearConnectorOperationsWithout(testCase.omit),
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
				t.Fatalf("preflightHeadlessCapabilityProfile code = %s, want FailedPrecondition; err=%v", status.Code(err), err)
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
			if diagnostic.GetReason() != "TRACKER_PLAN_ATTACHMENT_UNAVAILABLE" {
				t.Fatalf("diagnostic.Reason = %q, want %q", diagnostic.GetReason(), "TRACKER_PLAN_ATTACHMENT_UNAVAILABLE")
			}
			var missing []string
			if err := json.Unmarshal([]byte(diagnostic.GetMetadata()["missing"]), &missing); err != nil {
				t.Fatalf("decode missing metadata %q: %v", diagnostic.GetMetadata()["missing"], err)
			}
			if !slices.Equal(missing, testCase.wantMissing) {
				t.Fatalf("missing = %q, want %q", missing, testCase.wantMissing)
			}
		})
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
