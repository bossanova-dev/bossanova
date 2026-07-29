package main

import (
	"context"
	"slices"
	"testing"

	"github.com/rs/zerolog"

	bossanovav1 "github.com/recurser/bossalib/gen/bossanova/v1"
)

// newTestServer returns a Server wired to a nil host (the host client is only
// used by run-path RPCs these tests do not exercise) and a default Runner.
// Tests that need the introspection store point it at a fixture DB afterwards
// (see withFixtureStore), so no runner Options are needed here.
func newTestServer(t *testing.T) *Server {
	t.Helper()
	return newServer(nil, zerolog.Nop())
}

// TestGetInfo pins the opencode plugin's identity and its single foundation
// user setting: name "opencode", the agent_runner capability, and a STRING
// "model" setting. This is the acceptance contract for BOS-433 — the daemon
// enumerates plugins by this Info, and the settings UI surfaces the model knob.
func TestGetInfo(t *testing.T) {
	s := newTestServer(t)
	resp, err := s.GetInfo(context.Background(), &bossanovav1.AgentRunnerServiceGetInfoRequest{})
	if err != nil {
		t.Fatalf("GetInfo: %v", err)
	}
	if resp.Info == nil {
		t.Fatal("Info nil")
	}
	if resp.Info.Name != "opencode" {
		t.Errorf("Info.Name = %q, want opencode", resp.Info.Name)
	}

	if !slices.Contains(resp.Info.Capabilities, "agent_runner") {
		t.Errorf("Capabilities = %v, want to contain agent_runner", resp.Info.Capabilities)
	}

	model := userSettingByKey(resp.Info.UserSettings, "model")
	if model == nil {
		t.Fatalf("user setting %q missing from GetInfo", "model")
	}
	if model.Type != bossanovav1.UserSettingType_USER_SETTING_TYPE_STRING {
		t.Errorf("model.Type = %v, want USER_SETTING_TYPE_STRING", model.Type)
	}
	if len(model.AllowedValues) != 0 {
		t.Errorf("model.AllowedValues = %v, want empty (string-typed setting)", model.AllowedValues)
	}
}

func userSettingByKey(settings []*bossanovav1.UserSetting, key string) *bossanovav1.UserSetting {
	for _, us := range settings {
		if us.Key == key {
			return us
		}
	}
	return nil
}

// withFixtureStore points the server's introspection store at the vendored
// fixture DB (no subprocess / env probing) so the wired RPCs are exercised
// deterministically.
func withFixtureStore(t *testing.T, s *Server) {
	t.Helper()
	s.store = fixtureStore(t)
}

// TestIntrospectionRPCs_WiredToStore proves the four SQLite-backed RPCs return
// the store's values through the gRPC handler layer (request-field plumbing +
// response shapes), against the fixture DB.
func TestIntrospectionRPCs_WiredToStore(t *testing.T) {
	s := newTestServer(t)
	withFixtureStore(t, s)
	ctx := context.Background()

	title, err := s.GetChatTitle(ctx, &bossanovav1.GetChatTitleRequest{SessionId: "ses_fixturealpha00000000aa"})
	if err != nil {
		t.Fatalf("GetChatTitle: %v", err)
	}
	if !title.Supported {
		t.Error("GetChatTitle.Supported = false, want true")
	}
	if title.Title != "Fixture Alpha Session" {
		t.Errorf("GetChatTitle.Title = %q, want %q", title.Title, "Fixture Alpha Session")
	}
	if title.Explicit {
		t.Error("GetChatTitle.Explicit = true, want false")
	}

	exists, err := s.TranscriptExists(ctx, &bossanovav1.TranscriptExistsRequest{AgentSessionId: "ses_fixturebeta000000000bb"})
	if err != nil {
		t.Fatalf("TranscriptExists: %v", err)
	}
	if !exists.Exists {
		t.Error("TranscriptExists.Exists = false, want true")
	}

	lastUser, err := s.LastTurnIsUser(ctx, &bossanovav1.LastTurnIsUserRequest{AgentSessionId: "ses_fixturebeta000000000bb"})
	if err != nil {
		t.Fatalf("LastTurnIsUser: %v", err)
	}
	if !lastUser.IsUser {
		t.Error("LastTurnIsUser.IsUser = false, want true (beta ends with a user turn)")
	}

	resolved, err := s.ResolveInteractiveSessionID(ctx, &bossanovav1.ResolveInteractiveSessionIDRequest{WorkDir: "/tmp/bos435-fixture/shared"})
	if err != nil {
		t.Fatalf("ResolveInteractiveSessionID: %v", err)
	}
	if !resolved.Found || resolved.SessionId != "ses_fixtureshared_new0000dd" {
		t.Errorf("ResolveInteractiveSessionID = (%q, found=%v), want (ses_fixtureshared_new0000dd, true)",
			resolved.SessionId, resolved.Found)
	}
	if resolved.TranscriptPath != "" {
		t.Errorf("ResolveInteractiveSessionID.TranscriptPath = %q, want empty (opencode has no per-session file)", resolved.TranscriptPath)
	}
}

// TestIntrospectionRPCs_DegradeWhenNoDB proves the handlers never error and
// return zero-value payloads when the store cannot resolve a DB.
func TestIntrospectionRPCs_DegradeWhenNoDB(t *testing.T) {
	s := newTestServer(t)
	s.store = &opencodeStore{resolvePathFn: func() string { return "" }}
	ctx := context.Background()

	title, err := s.GetChatTitle(ctx, &bossanovav1.GetChatTitleRequest{SessionId: "ses_x"})
	if err != nil {
		t.Fatalf("GetChatTitle: %v", err)
	}
	if !title.Supported || title.Title != "" {
		t.Errorf("GetChatTitle = (supported=%v, title=%q), want (true, \"\")", title.Supported, title.Title)
	}

	exists, err := s.TranscriptExists(ctx, &bossanovav1.TranscriptExistsRequest{AgentSessionId: "ses_x"})
	if err != nil || exists.Exists {
		t.Errorf("TranscriptExists = (%v, err=%v), want (false, nil)", exists.GetExists(), err)
	}

	lastUser, err := s.LastTurnIsUser(ctx, &bossanovav1.LastTurnIsUserRequest{AgentSessionId: "ses_x"})
	if err != nil || lastUser.IsUser {
		t.Errorf("LastTurnIsUser = (%v, err=%v), want (false, nil)", lastUser.GetIsUser(), err)
	}

	resolved, err := s.ResolveInteractiveSessionID(ctx, &bossanovav1.ResolveInteractiveSessionIDRequest{WorkDir: "/tmp/x"})
	if err != nil || resolved.Found {
		t.Errorf("ResolveInteractiveSessionID = (found=%v, err=%v), want (false, nil)", resolved.GetFound(), err)
	}
}

// TestConstantRPCs pins the small remaining RPC surface: an empty (non-nil)
// dirty-files set, unsupported hook config, and a conservative no-question.
func TestConstantRPCs(t *testing.T) {
	s := newTestServer(t)
	ctx := context.Background()

	dirty, err := s.ListIgnoredDirtyFiles(ctx, &bossanovav1.ListIgnoredDirtyFilesRequest{})
	if err != nil {
		t.Fatalf("ListIgnoredDirtyFiles: %v", err)
	}
	if dirty.Paths == nil {
		t.Fatal("ListIgnoredDirtyFiles.Paths = nil, want non-nil slice")
	}
	// The injected BOS-486 question hook is bossd-written, not agent-authored:
	// it MUST be filtered out of finalize's `git status` read or a run that
	// changed nothing looks like it produced an untracked file. Fatal, not
	// Error: the path assertion below indexes the slice.
	const wantIgnored = ".opencode/plugins/bossd-question.js"
	if !slices.Contains(dirty.Paths, wantIgnored) {
		t.Fatalf("ListIgnoredDirtyFiles.Paths = %v, want to contain %q", dirty.Paths, wantIgnored)
	}
	// The path must match what installQuestionHook actually writes, expressed
	// relative to the worktree — a drift here silently un-ignores the file.
	if got := questionHookPath("/wt"); got != "/wt/"+wantIgnored {
		t.Errorf("ignored path %q does not match the injected path %q", wantIgnored, got)
	}

	fin, err := s.ConfigureFinalizeHook(ctx, &bossanovav1.ConfigureFinalizeHookRequest{})
	if err != nil {
		t.Fatalf("ConfigureFinalizeHook: %v", err)
	}
	if fin.IsSupported {
		t.Error("ConfigureFinalizeHook.IsSupported = true, want false")
	}

	// RemoveAgentRunHook now reports supported: BOS-486 gave opencode a real
	// run-scoped hook to remove (the injected .opencode/plugins asset). Its
	// IsSupported is NOT a bossd routing flag — unlike ConfigureFinalizeHook's,
	// which is pinned false by TestConfigureFinalizeHookStaysUnsupported. It is
	// exercised against a real worktree in TestRemoveAgentRunHookDeletesAsset.
	rm, err := s.RemoveAgentRunHook(ctx, &bossanovav1.RemoveAgentRunHookRequest{})
	if err != nil {
		t.Fatalf("RemoveAgentRunHook: %v", err)
	}
	if !rm.IsSupported {
		t.Error("RemoveAgentRunHook.IsSupported = false, want true")
	}

	// HasQuestionPrompt stays false as a DELIBERATE decision rather than an
	// unimplemented stub: opencode is headless, so the pane-regex fallback this
	// RPC backs has nothing to scrape. The BOS-486 question signal reaches bossd
	// out-of-band, over the loopback receiver, not through here. Flipping this
	// to true means that has changed — re-read
	// docs/solutions/logic-errors/spike-opencode-question-signal-events-unreachable.md
	// before doing so.
	q, err := s.HasQuestionPrompt(ctx, &bossanovav1.HasQuestionPromptRequest{})
	if err != nil {
		t.Fatalf("HasQuestionPrompt: %v", err)
	}
	if q.HasPrompt {
		t.Error("HasQuestionPrompt.HasPrompt = true, want false")
	}
}
