package agent

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"github.com/recurser/bossalib/agenttelemetry"
	bossanovav1 "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossd/internal/db"
)

// fakeAgentClient implements AgentRunnerClient for tests. Each method
// records the request and returns the configured response/error.
type fakeAgentClient struct {
	startResp    *bossanovav1.StartAgentRunResponse
	startFn      func(context.Context, *bossanovav1.StartAgentRunRequest) (*bossanovav1.StartAgentRunResponse, error)
	startErr     error
	startReq     atomic.Pointer[bossanovav1.StartAgentRunRequest]
	preflightErr error
	preflightReq atomic.Pointer[bossanovav1.PreflightHeadlessRunRequest]
	stopErr      error
	running      bool
	exitError    string
}

func (f *fakeAgentClient) GetInfo(context.Context) (*bossanovav1.PluginInfo, error) {
	return &bossanovav1.PluginInfo{Name: "fake"}, nil
}
func (f *fakeAgentClient) StartRun(_ context.Context, req *bossanovav1.StartAgentRunRequest) (*bossanovav1.StartAgentRunResponse, error) {
	f.startReq.Store(req)
	if f.startFn != nil {
		return f.startFn(context.Background(), req)
	}
	return f.startResp, f.startErr
}
func (f *fakeAgentClient) PreflightHeadlessRun(_ context.Context, req *bossanovav1.PreflightHeadlessRunRequest) (*bossanovav1.PreflightHeadlessRunResponse, error) {
	f.preflightReq.Store(req)
	return &bossanovav1.PreflightHeadlessRunResponse{}, f.preflightErr
}
func (f *fakeAgentClient) StopRun(_ context.Context, _ *bossanovav1.StopAgentRunRequest) (*bossanovav1.StopAgentRunResponse, error) {
	return &bossanovav1.StopAgentRunResponse{}, f.stopErr
}
func (f *fakeAgentClient) IsRunning(_ context.Context, _ *bossanovav1.IsAgentRunningRequest) (*bossanovav1.IsAgentRunningResponse, error) {
	return &bossanovav1.IsAgentRunningResponse{Running: f.running}, nil
}
func (f *fakeAgentClient) ExitStatus(_ context.Context, _ *bossanovav1.AgentExitStatusRequest) (*bossanovav1.AgentExitStatusResponse, error) {
	return &bossanovav1.AgentExitStatusResponse{IsComplete: !f.running, ExitError: f.exitError}, nil
}
func (f *fakeAgentClient) ConfigureFinalizeHook(context.Context, *bossanovav1.ConfigureFinalizeHookRequest) (*bossanovav1.ConfigureFinalizeHookResponse, error) {
	return &bossanovav1.ConfigureFinalizeHookResponse{IsSupported: true}, nil
}
func (f *fakeAgentClient) RemoveAgentRunHook(context.Context, *bossanovav1.RemoveAgentRunHookRequest) (*bossanovav1.RemoveAgentRunHookResponse, error) {
	return &bossanovav1.RemoveAgentRunHookResponse{IsSupported: true}, nil
}
func (f *fakeAgentClient) BuildInteractiveCommand(context.Context, *bossanovav1.BuildInteractiveCommandRequest) (*bossanovav1.BuildInteractiveCommandResponse, error) {
	return &bossanovav1.BuildInteractiveCommandResponse{Argv: []string{"sh", "-c", "true"}}, nil
}
func (f *fakeAgentClient) ResolveInteractiveSessionID(context.Context, *bossanovav1.ResolveInteractiveSessionIDRequest) (*bossanovav1.ResolveInteractiveSessionIDResponse, error) {
	return &bossanovav1.ResolveInteractiveSessionIDResponse{}, nil
}
func (f *fakeAgentClient) ListIgnoredDirtyFiles(context.Context, *bossanovav1.ListIgnoredDirtyFilesRequest) (*bossanovav1.ListIgnoredDirtyFilesResponse, error) {
	return &bossanovav1.ListIgnoredDirtyFilesResponse{Paths: []string{".claude/settings.local.json"}}, nil
}
func (f *fakeAgentClient) SuggestPRTitle(context.Context, *bossanovav1.SuggestPRTitleRequest) (*bossanovav1.SuggestPRTitleResponse, error) {
	return &bossanovav1.SuggestPRTitleResponse{}, nil
}

func (f *fakeAgentClient) GetChatTitle(context.Context, *bossanovav1.GetChatTitleRequest) (*bossanovav1.GetChatTitleResponse, error) {
	return &bossanovav1.GetChatTitleResponse{Supported: true, Title: ""}, nil
}
func (f *fakeAgentClient) HasQuestionPrompt(context.Context, *bossanovav1.HasQuestionPromptRequest) (*bossanovav1.HasQuestionPromptResponse, error) {
	return &bossanovav1.HasQuestionPromptResponse{}, nil
}

func (f *fakeAgentClient) DetectUsageLimit(context.Context, *bossanovav1.DetectUsageLimitRequest) (*bossanovav1.DetectUsageLimitResponse, error) {
	return &bossanovav1.DetectUsageLimitResponse{}, nil
}

func (f *fakeAgentClient) ProbeRateLimit(context.Context, *bossanovav1.ProbeRateLimitRequest) (*bossanovav1.ProbeRateLimitResponse, error) {
	return &bossanovav1.ProbeRateLimitResponse{}, nil
}

func (f *fakeAgentClient) HasWorkingIndicator(context.Context, *bossanovav1.HasWorkingIndicatorRequest) (*bossanovav1.HasWorkingIndicatorResponse, error) {
	return &bossanovav1.HasWorkingIndicatorResponse{}, nil
}
func (f *fakeAgentClient) LastTurnIsUser(context.Context, *bossanovav1.LastTurnIsUserRequest) (*bossanovav1.LastTurnIsUserResponse, error) {
	return &bossanovav1.LastTurnIsUserResponse{}, nil
}
func (f *fakeAgentClient) TranscriptExists(context.Context, *bossanovav1.TranscriptExistsRequest) (*bossanovav1.TranscriptExistsResponse, error) {
	return &bossanovav1.TranscriptExistsResponse{}, nil
}
func (f *fakeAgentClient) ReadTranscript(context.Context, *bossanovav1.ReadTranscriptRequest) (*bossanovav1.ReadTranscriptResponse, error) {
	return &bossanovav1.ReadTranscriptResponse{}, nil
}
func (f *fakeAgentClient) RotationCapability(context.Context, *bossanovav1.RotationCapabilityRequest) (*bossanovav1.RotationCapabilityResponse, error) {
	return &bossanovav1.RotationCapabilityResponse{}, nil
}
func (f *fakeAgentClient) MaterializeAccount(context.Context, *bossanovav1.MaterializeAccountRequest) (*bossanovav1.MaterializeAccountResponse, error) {
	return &bossanovav1.MaterializeAccountResponse{}, nil
}

type fakeAgentRunStore struct {
	starts          []db.AgentRun
	startCtxErr     error
	startDeadline   time.Time
	stops           []string
	stopCtxErr      error
	telemetryCtxErr error
	telemetryKey    string
	telemetry       db.AgentRunTelemetry
	byRunID         bool
	calls           []string
}

func (f *fakeAgentRunStore) Start(ctx context.Context, run db.AgentRun) (db.AgentRun, error) {
	f.startCtxErr = ctx.Err()
	f.startDeadline, _ = ctx.Deadline()
	if run.ID == "" {
		run.ID = fmt.Sprintf("run-%d", len(f.starts)+1)
	}
	f.starts = append(f.starts, run)
	return run, nil
}

func (f *fakeAgentRunStore) Stop(ctx context.Context, agentSessionID, _ string, _ time.Time) error {
	f.stopCtxErr = ctx.Err()
	f.stops = append(f.stops, agentSessionID)
	f.calls = append(f.calls, "stop")
	return nil
}

func (f *fakeAgentRunStore) StopRun(ctx context.Context, runID, reason string, stoppedAt time.Time) error {
	return f.Stop(ctx, runID, reason, stoppedAt)
}

func (f *fakeAgentRunStore) RecordTelemetry(ctx context.Context, runID string, telemetry db.AgentRunTelemetry) error {
	f.telemetryCtxErr = ctx.Err()
	f.telemetryKey = runID
	f.telemetry = telemetry
	f.byRunID = true
	f.calls = append(f.calls, "telemetry")
	return nil
}

func (f *fakeAgentRunStore) RecordTelemetryByAgentSessionID(ctx context.Context, agentSessionID string, telemetry db.AgentRunTelemetry) error {
	f.telemetryCtxErr = ctx.Err()
	f.telemetryKey = agentSessionID
	f.telemetry = telemetry
	f.byRunID = false
	f.calls = append(f.calls, "telemetry")
	return nil
}

func (f *fakeAgentRunStore) ReconcileOpen(context.Context, time.Time, []string) (int64, error) {
	return 0, nil
}

func (f *fakeAgentRunStore) List(context.Context, db.AgentRunFilter) ([]db.AgentRun, error) {
	return nil, nil
}

func (f *fakeAgentRunStore) Backfill(context.Context, db.AgentRunBackfillParams) (db.AgentRunBackfillSummary, error) {
	return db.AgentRunBackfillSummary{}, nil
}

func TestPluginRunner_Start_ResolvesLogPath(t *testing.T) {
	fc := &fakeAgentClient{startResp: &bossanovav1.StartAgentRunResponse{SessionId: "sid"}}
	tl := NewTailer(zerolog.Nop())
	logDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(logDir, "explicit-sid.log"), nil, 0o600); err != nil {
		t.Fatalf("seed log: %v", err)
	}
	pr := NewPluginRunner(fc, tl, logDir, zerolog.Nop())

	sid, err := pr.Start(context.Background(), "/work", "plan", nil, "explicit-sid", "", "", nil)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if sid != "sid" {
		t.Errorf("returned sid = %q, want sid", sid)
	}
	got := fc.startReq.Load()
	if got == nil {
		t.Fatal("StartRun req not recorded")
	}
	if got.WorkDir != "/work" || got.Plan != "plan" || got.SessionId != "explicit-sid" {
		t.Errorf("unexpected req: %+v", got)
	}
	if got.LogPath != filepath.Join(logDir, "explicit-sid.log") {
		t.Errorf("LogPath = %q, want explicit session path", got.LogPath)
	}
}

func TestPluginRunner_Start_TailsRequestLogPathUnderReturnedSessionID(t *testing.T) {
	const lineText = "line written to requested path"
	fc := &fakeAgentClient{startResp: &bossanovav1.StartAgentRunResponse{SessionId: "sid"}}
	fc.startFn = func(_ context.Context, req *bossanovav1.StartAgentRunRequest) (*bossanovav1.StartAgentRunResponse, error) {
		line := fmt.Sprintf(`{"ts":"2026-08-22T17:00:00Z","text":%q}`+"\n", lineText)
		if err := os.WriteFile(req.LogPath, []byte(line), 0o600); err != nil {
			return nil, err
		}
		return fc.startResp, nil
	}
	pr := NewPluginRunner(fc, NewTailer(zerolog.Nop()), t.TempDir(), zerolog.Nop())

	sid, err := pr.Start(context.Background(), "/work", "plan", nil, "explicit-sid", "", "", nil)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if sid != "sid" {
		t.Fatalf("sid = %q, want sid", sid)
	}
	if _, err := pr.Subscribe(context.Background(), sid); err != nil {
		t.Fatalf("Subscribe returned id: %v", err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		for _, line := range pr.History(sid) {
			if line.Text == lineText {
				return
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("History(%q) never observed %q; got %#v", sid, lineText, pr.History(sid))
}

func TestPluginRunner_ExitErrorRecordsRunTelemetryFromLog(t *testing.T) {
	fc := &fakeAgentClient{startResp: &bossanovav1.StartAgentRunResponse{SessionId: "sid"}}
	logDir := t.TempDir()
	fc.startFn = func(_ context.Context, req *bossanovav1.StartAgentRunRequest) (*bossanovav1.StartAgentRunResponse, error) {
		line := codexAssistantLogLine(time.Now())
		if err := os.WriteFile(req.LogPath, []byte(line), 0o600); err != nil {
			return nil, err
		}
		return fc.startResp, nil
	}
	runs := &fakeAgentRunStore{}
	pr := NewPluginRunner(fc, NewTailer(zerolog.Nop()), logDir, zerolog.Nop())
	pr.SetAgentName("codex")
	pr.SetAgentRunStore(runs)

	if _, err := pr.Start(context.Background(), "/work", "plan", nil, "session-1", "gpt-5", "high", nil); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if err := pr.ExitError("sid"); err != nil {
		t.Fatalf("ExitError: %v", err)
	}
	if len(runs.starts) != 1 || runs.starts[0].AgentSessionID != "sid" || runs.starts[0].SessionID != "session-1" {
		t.Fatalf("starts = %#v, want one start for sid/session-1", runs.starts)
	}
	if len(runs.stops) != 1 || runs.stops[0] != "sid" {
		t.Fatalf("stops = %#v, want sid", runs.stops)
	}
	if strings.Join(runs.calls, ",") != "stop,telemetry" {
		t.Fatalf("persistence calls = %#v, want stop before telemetry", runs.calls)
	}
	if !runs.byRunID || runs.telemetryKey != "run-1" || runs.telemetry.ParentModelCallCount != 1 {
		t.Fatalf("telemetry byRunID=%v key=%q counts=%#v, want run-1 with one parent call", runs.byRunID, runs.telemetryKey, runs.telemetry)
	}
	assertRunMemoryCleared(t, pr, "sid")
}

func TestPluginRunner_StartDetachesAgentRunStartFromCallerCancellation(t *testing.T) {
	fc := &fakeAgentClient{startResp: &bossanovav1.StartAgentRunResponse{SessionId: "sid"}}
	runs := &fakeAgentRunStore{}
	pr := NewPluginRunner(fc, NewTailer(zerolog.Nop()), t.TempDir(), zerolog.Nop())
	pr.SetAgentName("codex")
	pr.SetAgentRunStore(runs)
	ctx, cancel := context.WithCancel(context.Background())
	fc.startFn = func(_ context.Context, req *bossanovav1.StartAgentRunRequest) (*bossanovav1.StartAgentRunResponse, error) {
		cancel()
		return fc.startResp, nil
	}

	if _, err := pr.Start(ctx, "/work", "plan", nil, "session-1", "gpt-5", "high", nil); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if len(runs.starts) != 1 {
		t.Fatalf("starts = %d, want 1", len(runs.starts))
	}
	if runs.startCtxErr != nil {
		t.Fatalf("start ctx err = %v, want nil detached context", runs.startCtxErr)
	}
	if runs.startDeadline.IsZero() {
		t.Fatal("start context deadline is zero, want bounded detached context")
	}
}

func TestTallyAgentLogHonorsCanceledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := tallyAgentLog(ctx, filepath.Join(t.TempDir(), "missing.jsonl"), "sid", "", time.Time{})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("tallyAgentLog err = %v, want context.Canceled", err)
	}
}

func TestPluginRunner_StartWithHeadlessLaunchOptionsRecordsBossSessionID(t *testing.T) {
	fc := &fakeAgentClient{startResp: &bossanovav1.StartAgentRunResponse{SessionId: "provider-session"}}
	var startReturningAt time.Time
	fc.startFn = func(_ context.Context, req *bossanovav1.StartAgentRunRequest) (*bossanovav1.StartAgentRunResponse, error) {
		time.Sleep(20 * time.Millisecond)
		startReturningAt = time.Now()
		return fc.startResp, nil
	}
	runs := &fakeAgentRunStore{}
	logDir := t.TempDir()
	pr := NewPluginRunner(fc, NewTailer(zerolog.Nop()), logDir, zerolog.Nop())
	pr.SetAgentName("codex")
	pr.SetAgentRunStore(runs)

	sid, err := pr.StartWithHeadlessLaunchOptions(context.Background(), "/work", "plan", nil, "", "", "high", nil, HeadlessLaunchOptions{
		BossSessionID:  "boss-session",
		EffectiveModel: "configured-model",
	})
	if err != nil {
		t.Fatalf("StartWithHeadlessLaunchOptions: %v", err)
	}
	if sid != "provider-session" {
		t.Fatalf("sid = %q, want provider-session", sid)
	}
	gotReq := fc.startReq.Load()
	if gotReq == nil || gotReq.GetSessionId() != "" {
		t.Fatalf("plugin session id = %#v, want empty provider key for fresh run", gotReq)
	}
	if gotReq.GetLogPath() != filepath.Join(logDir, "boss-session.log") {
		t.Fatalf("plugin log path = %q, want Boss session keyed log path", gotReq.GetLogPath())
	}
	if len(runs.starts) != 1 || runs.starts[0].SessionID != "boss-session" || runs.starts[0].AgentSessionID != "provider-session" {
		t.Fatalf("starts = %#v, want boss session id plus provider agent session id", runs.starts)
	}
	if runs.starts[0].Model != "configured-model" {
		t.Fatalf("recorded model = %q, want effective configured model", runs.starts[0].Model)
	}
	if runs.starts[0].AgentName != "codex" {
		t.Fatalf("recorded agent name = %q, want codex", runs.starts[0].AgentName)
	}
	if runs.starts[0].StartedAt.After(startReturningAt) {
		t.Fatalf("recorded start = %s, want no later than StartRun return %s", runs.starts[0].StartedAt, startReturningAt)
	}
}

func TestPluginRunner_StopRecordsTelemetryFromLog(t *testing.T) {
	fc := &fakeAgentClient{startResp: &bossanovav1.StartAgentRunResponse{SessionId: "sid"}}
	logDir := t.TempDir()
	fc.startFn = func(_ context.Context, req *bossanovav1.StartAgentRunRequest) (*bossanovav1.StartAgentRunResponse, error) {
		line := codexAssistantLogLine(time.Now())
		if err := os.WriteFile(req.LogPath, []byte(line), 0o600); err != nil {
			return nil, err
		}
		return fc.startResp, nil
	}
	runs := &fakeAgentRunStore{}
	pr := NewPluginRunner(fc, NewTailer(zerolog.Nop()), logDir, zerolog.Nop())
	pr.SetAgentRunStore(runs)

	if _, err := pr.Start(context.Background(), "/work", "plan", nil, "session-1", "gpt-5", "high", nil); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if err := pr.StopWithContext(context.Background(), "sid"); err != nil {
		t.Fatalf("StopWithContext: %v", err)
	}
	if len(runs.stops) != 1 || runs.stops[0] != "sid" {
		t.Fatalf("stops = %#v, want sid", runs.stops)
	}
	if !runs.byRunID || runs.telemetryKey != "run-1" || runs.telemetry.ParentModelCallCount != 1 {
		t.Fatalf("telemetry byRunID=%v key=%q counts=%#v, want run-1 with one parent call", runs.byRunID, runs.telemetryKey, runs.telemetry)
	}
	assertRunMemoryCleared(t, pr, "sid")
}

func TestPluginRunner_StopErrorKeepsRunOpenAndPersistsTelemetryWithFreshContext(t *testing.T) {
	fc := &fakeAgentClient{startResp: &bossanovav1.StartAgentRunResponse{SessionId: "sid"}, stopErr: context.Canceled}
	logDir := t.TempDir()
	fc.startFn = func(_ context.Context, req *bossanovav1.StartAgentRunRequest) (*bossanovav1.StartAgentRunResponse, error) {
		line := codexAssistantLogLine(time.Now())
		if err := os.WriteFile(req.LogPath, []byte(line), 0o600); err != nil {
			return nil, err
		}
		return fc.startResp, nil
	}
	runs := &fakeAgentRunStore{}
	pr := NewPluginRunner(fc, NewTailer(zerolog.Nop()), logDir, zerolog.Nop())
	pr.SetAgentRunStore(runs)

	if _, err := pr.Start(context.Background(), "/work", "plan", nil, "session-1", "gpt-5", "high", nil); err != nil {
		t.Fatalf("Start: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := pr.StopWithContext(ctx, "sid"); err == nil {
		t.Fatal("StopWithContext succeeded, want plugin stop error")
	}
	if len(runs.stops) != 0 || runs.telemetryKey != "run-1" || strings.Join(runs.calls, ",") != "telemetry" {
		t.Fatalf("persistence calls stops=%#v calls=%#v telemetryKey=%q, want telemetry without stop", runs.stops, runs.calls, runs.telemetryKey)
	}
	if runs.telemetryCtxErr != nil {
		t.Fatalf("telemetry context = %v, want fresh live context", runs.telemetryCtxErr)
	}
	assertRunMemoryRetained(t, pr, "sid")
}

func TestPluginRunner_ExitErrorRecordsClaudeChildSidecarsFromLiveLog(t *testing.T) {
	fc := &fakeAgentClient{startResp: &bossanovav1.StartAgentRunResponse{SessionId: "parent"}}
	logDir := t.TempDir()
	fc.startFn = func(_ context.Context, req *bossanovav1.StartAgentRunRequest) (*bossanovav1.StartAgentRunResponse, error) {
		if err := os.WriteFile(req.LogPath, []byte(`{"timestamp":"2026-08-26T01:00:00Z","type":"assistant","message":{"role":"assistant","usage":{"output_tokens":10}}}`+"\n"), 0o600); err != nil {
			return nil, err
		}
		childDir := filepath.Join(filepath.Dir(req.LogPath), "parent", "subagents")
		if err := os.MkdirAll(childDir, 0o700); err != nil {
			return nil, err
		}
		if err := os.WriteFile(filepath.Join(childDir, "child.jsonl"), []byte(`{"timestamp":"2026-08-26T01:01:00Z","type":"assistant","message":{"role":"assistant","usage":{"output_tokens":7}}}`+"\n"), 0o600); err != nil {
			return nil, err
		}
		if err := os.WriteFile(filepath.Join(childDir, "child.meta.json"), []byte(`{"parentAgentId":"parent","spawnDepth":1}`), 0o600); err != nil {
			return nil, err
		}
		return fc.startResp, nil
	}
	runs := &fakeAgentRunStore{}
	pr := NewPluginRunner(fc, NewTailer(zerolog.Nop()), logDir, zerolog.Nop())
	pr.SetAgentRunStore(runs)

	if _, err := pr.Start(context.Background(), "/work", "plan", nil, "session-1", "gpt-5", "high", nil); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if err := pr.ExitError("parent"); err != nil {
		t.Fatalf("ExitError: %v", err)
	}
	if !runs.byRunID || runs.telemetry.ChildModelCallCount != 1 || runs.telemetry.DirectSubagentCount != 1 {
		t.Fatalf("telemetry byRunID=%v counts=%#v, want child/direct counts", runs.byRunID, runs.telemetry)
	}
	if len(runs.telemetry.Children) != 1 || runs.telemetry.Children[0].AgentSessionID != "child" {
		t.Fatalf("children = %#v, want child sidecar", runs.telemetry.Children)
	}
	if runs.telemetry.OutputTokenCount == nil || *runs.telemetry.OutputTokenCount != 17 {
		t.Fatalf("output tokens = %v, want 17", runs.telemetry.OutputTokenCount)
	}
}

func TestPluginRunner_ExitErrorRecordsClaudeSidecarsFromTranscriptDirectory(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	workDir := filepath.Join(home, "worktree")
	fc := &fakeAgentClient{startResp: &bossanovav1.StartAgentRunResponse{SessionId: "parent"}}
	logDir := t.TempDir()
	fc.startFn = func(_ context.Context, req *bossanovav1.StartAgentRunRequest) (*bossanovav1.StartAgentRunResponse, error) {
		oldParentAt := time.Now().Add(-24 * time.Hour).UTC().Format(time.RFC3339Nano)
		currentParentAt := time.Now().Add(time.Second).UTC().Format(time.RFC3339Nano)
		currentChildAt := time.Now().Add(2 * time.Second).UTC().Format(time.RFC3339Nano)
		if err := os.WriteFile(req.LogPath, []byte("raw pane output\n"), 0o600); err != nil {
			return nil, err
		}
		transcript, err := agenttelemetry.ClaudeTranscriptPath(workDir, "parent")
		if err != nil {
			return nil, err
		}
		if err := os.MkdirAll(filepath.Join(filepath.Dir(transcript), "parent", "subagents"), 0o700); err != nil {
			return nil, err
		}
		if err := os.WriteFile(transcript, []byte(strings.Join([]string{
			`{"timestamp":` + strconv.Quote(oldParentAt) + `,"type":"assistant","message":{"role":"assistant","usage":{"output_tokens":99}}}`,
			`{"timestamp":` + strconv.Quote(currentParentAt) + `,"type":"assistant","message":{"role":"assistant","usage":{"output_tokens":10}}}`,
		}, "\n")), 0o600); err != nil {
			return nil, err
		}
		childDir := filepath.Join(filepath.Dir(transcript), "parent", "subagents")
		if err := os.WriteFile(filepath.Join(childDir, "child.jsonl"), []byte(`{"timestamp":`+strconv.Quote(currentChildAt)+`,"type":"assistant","message":{"role":"assistant","usage":{"output_tokens":7}}}`+"\n"), 0o600); err != nil {
			return nil, err
		}
		if err := os.WriteFile(filepath.Join(childDir, "child.meta.json"), []byte(`{"parentAgentId":"parent","spawnDepth":1}`), 0o600); err != nil {
			return nil, err
		}
		return fc.startResp, nil
	}
	runs := &fakeAgentRunStore{}
	pr := NewPluginRunner(fc, NewTailer(zerolog.Nop()), logDir, zerolog.Nop())
	pr.SetAgentRunStore(runs)

	if _, err := pr.Start(context.Background(), workDir, "plan", nil, "session-1", "gpt-5", "high", nil); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if err := pr.ExitError("parent"); err != nil {
		t.Fatalf("ExitError: %v", err)
	}
	if runs.telemetry.ChildModelCallCount != 1 || len(runs.telemetry.Children) != 1 {
		t.Fatalf("telemetry = %#v, want child sidecar from transcript directory", runs.telemetry)
	}
	if runs.telemetry.OutputTokenCount == nil || *runs.telemetry.OutputTokenCount != 17 {
		t.Fatalf("output tokens = %v, want 17 from transcript plus child", runs.telemetry.OutputTokenCount)
	}
	assertRunMemoryCleared(t, pr, "parent")
}

func TestPluginRunner_ExitErrorWithPluginErrorClearsRunMemoryAfterTelemetry(t *testing.T) {
	fc := &fakeAgentClient{
		startResp: &bossanovav1.StartAgentRunResponse{SessionId: "sid"},
		exitError: "usage limit reached",
	}
	fc.startFn = func(_ context.Context, req *bossanovav1.StartAgentRunRequest) (*bossanovav1.StartAgentRunResponse, error) {
		line := codexAssistantLogLine(time.Now())
		if err := os.WriteFile(req.LogPath, []byte(line), 0o600); err != nil {
			return nil, err
		}
		return fc.startResp, nil
	}
	runs := &fakeAgentRunStore{}
	pr := NewPluginRunner(fc, NewTailer(zerolog.Nop()), t.TempDir(), zerolog.Nop())
	pr.SetAgentRunStore(runs)

	if _, err := pr.Start(context.Background(), "/work", "plan", nil, "session-1", "gpt-5", "high", nil); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if err := pr.ExitError("sid"); err == nil {
		t.Fatal("ExitError succeeded, want plugin exit error")
	}
	if runs.telemetryKey != "run-1" || runs.telemetry.ParentModelCallCount != 1 {
		t.Fatalf("telemetry key=%q counts=%#v, want run-1 with one parent call", runs.telemetryKey, runs.telemetry)
	}
	assertRunMemoryCleared(t, pr, "sid")
}

func assertRunMemoryCleared(t *testing.T, pr *PluginRunner, sessionID string) {
	t.Helper()
	pr.logMu.Lock()
	defer pr.logMu.Unlock()
	if pr.logPaths[sessionID] != "" || pr.runIDs[sessionID] != "" || pr.workDirs[sessionID] != "" || !pr.starts[sessionID].IsZero() {
		t.Fatalf("run memory for %q retained: logPath=%q runID=%q workDir=%q started=%s", sessionID, pr.logPaths[sessionID], pr.runIDs[sessionID], pr.workDirs[sessionID], pr.starts[sessionID])
	}
}

func codexAssistantLogLine(ts time.Time) string {
	formatted := ts.UTC().Format(time.RFC3339Nano)
	inner := `{"timestamp":` + strconv.Quote(formatted) + `,"type":"assistant_message"}`
	return `{"ts":` + strconv.Quote(formatted) + `,"text":` + strconv.Quote(inner) + `}` + "\n"
}

func assertRunMemoryRetained(t *testing.T, pr *PluginRunner, sessionID string) {
	t.Helper()
	pr.logMu.Lock()
	defer pr.logMu.Unlock()
	if pr.logPaths[sessionID] == "" || pr.runIDs[sessionID] == "" || pr.workDirs[sessionID] == "" || pr.starts[sessionID].IsZero() {
		t.Fatalf("run memory for %q was not retained: logPath=%q runID=%q workDir=%q started=%s", sessionID, pr.logPaths[sessionID], pr.runIDs[sessionID], pr.workDirs[sessionID], pr.starts[sessionID])
	}
}

func TestPluginRunner_Start_EmptySessionIDUsesDistinctLogPaths(t *testing.T) {
	logDir := t.TempDir()
	var starts atomic.Int64
	paths := make(chan string, 2)
	fc := &fakeAgentClient{}
	fc.startFn = func(_ context.Context, req *bossanovav1.StartAgentRunRequest) (*bossanovav1.StartAgentRunResponse, error) {
		if err := os.WriteFile(req.LogPath, nil, 0o600); err != nil {
			return nil, err
		}
		paths <- req.LogPath
		return &bossanovav1.StartAgentRunResponse{SessionId: fmt.Sprintf("sid-%d", starts.Add(1))}, nil
	}
	pr := NewPluginRunner(fc, NewTailer(zerolog.Nop()), logDir, zerolog.Nop())

	errs := make(chan error, 2)
	for range 2 {
		go func() {
			_, err := pr.Start(context.Background(), "/work", "plan", nil, "", "", "", nil)
			errs <- err
		}()
	}
	for range 2 {
		if err := <-errs; err != nil {
			t.Fatalf("Start: %v", err)
		}
	}
	first, second := <-paths, <-paths
	if first == second {
		t.Fatalf("empty-session starts used same LogPath %q", first)
	}
	for _, path := range []string{first, second} {
		if path == filepath.Join(logDir, ".log") {
			t.Fatalf("empty-session start used collision path %q", path)
		}
		if !strings.HasPrefix(path, logDir+string(os.PathSeparator)) {
			t.Fatalf("LogPath %q is outside log dir %q", path, logDir)
		}
	}
}

func TestPluginRunner_Start_CarriesExtraEnv(t *testing.T) {
	fc := &fakeAgentClient{startResp: &bossanovav1.StartAgentRunResponse{SessionId: "sid"}}
	pr := NewPluginRunner(fc, NewTailer(zerolog.Nop()), t.TempDir(), zerolog.Nop())

	extra := map[string]string{
		"PROOF_ANTHROPIC_API_KEY": "secret-value",
		"BOSS_PROOF_R2_BUCKET":    "bossanova-proof-production",
	}
	if _, err := pr.Start(context.Background(), "/work", "plan", nil, "sid", "", "", extra); err != nil {
		t.Fatalf("Start: %v", err)
	}
	got := fc.startReq.Load()
	if got == nil {
		t.Fatal("StartRun req not recorded")
	}
	if got.GetExtraEnv()["PROOF_ANTHROPIC_API_KEY"] != "secret-value" {
		t.Errorf("ExtraEnv secret not forwarded: %v", got.GetExtraEnv())
	}
	if got.GetExtraEnv()["BOSS_PROOF_R2_BUCKET"] != "bossanova-proof-production" {
		t.Errorf("ExtraEnv constant not forwarded: %v", got.GetExtraEnv())
	}
}

func TestPluginRunner_StartCarriesModelAndEffort(t *testing.T) {
	fc := &fakeAgentClient{startResp: &bossanovav1.StartAgentRunResponse{SessionId: "sid"}}
	pr := NewPluginRunner(fc, NewTailer(zerolog.Nop()), t.TempDir(), zerolog.Nop())

	if _, err := pr.Start(context.Background(), "/work", "plan", nil, "sid", "opus", "high", nil); err != nil {
		t.Fatalf("Start: %v", err)
	}
	got := fc.startReq.Load()
	if got == nil {
		t.Fatal("StartRun req not recorded")
	}
	if got.GetModel() != "opus" || got.GetEffort() != "high" {
		t.Fatalf("target fields = model=%q effort=%q, want opus/high", got.GetModel(), got.GetEffort())
	}
}

func TestPluginRunner_StartWithHeadlessCapabilityProfileCarriesProfile(t *testing.T) {
	fc := &fakeAgentClient{startResp: &bossanovav1.StartAgentRunResponse{SessionId: "sid"}}
	pr := NewPluginRunner(fc, NewTailer(zerolog.Nop()), t.TempDir(), zerolog.Nop())

	if _, err := pr.StartWithHeadlessCapabilityProfile(
		context.Background(), "/work", "plan", nil, "sid", "model-for-preflight", "medium", map[string]string{"CODEX_HOME": "/projected/home"},
		bossanovav1.HeadlessCapabilityProfile_HEADLESS_CAPABILITY_PROFILE_TRACKER_PLAN_ATTACHMENT_V1,
	); err != nil {
		t.Fatalf("StartWithHeadlessCapabilityProfile: %v", err)
	}
	got := fc.startReq.Load()
	if got == nil {
		t.Fatal("StartRun req not recorded")
	}
	if got.GetHeadlessCapabilityProfile() != bossanovav1.HeadlessCapabilityProfile_HEADLESS_CAPABILITY_PROFILE_TRACKER_PLAN_ATTACHMENT_V1 {
		t.Fatalf("HeadlessCapabilityProfile = %s, want tracker-plan-attachment-v1", got.GetHeadlessCapabilityProfile())
	}
	if got.GetModel() != "model-for-preflight" || got.GetEffort() != "medium" || got.GetExtraEnv()["CODEX_HOME"] != "/projected/home" {
		t.Fatalf("preflight target fields = model=%q effort=%q env=%v", got.GetModel(), got.GetEffort(), got.GetExtraEnv())
	}
}

func TestPluginRunner_PreflightHeadlessCapabilityProfileCarriesOnlyTargetInputs(t *testing.T) {
	fc := &fakeAgentClient{}
	pr := NewPluginRunner(fc, NewTailer(zerolog.Nop()), t.TempDir(), zerolog.Nop())

	err := pr.PreflightHeadlessCapabilityProfile(
		context.Background(),
		"/worktrees/gated-run",
		"model-for-preflight",
		"medium",
		map[string]string{"CODEX_HOME": "/projected/home", "ACCOUNT_TOKEN": "secret"},
		bossanovav1.HeadlessCapabilityProfile_HEADLESS_CAPABILITY_PROFILE_TRACKER_PLAN_ATTACHMENT_V1,
	)
	if err != nil {
		t.Fatalf("PreflightHeadlessCapabilityProfile: %v", err)
	}
	got := fc.preflightReq.Load()
	if got == nil {
		t.Fatal("PreflightHeadlessRun req not recorded")
	}
	if got.GetModel() != "model-for-preflight" {
		t.Fatalf("Model = %q, want model-for-preflight", got.GetModel())
	}
	if got.GetEffort() != "medium" {
		t.Fatalf("Effort = %q, want medium", got.GetEffort())
	}
	// The gated run's working directory must reach the plugin, or the plugin
	// profiles a runtime that never loaded the repo's own agent config.
	if got.GetWorkDir() != "/worktrees/gated-run" {
		t.Fatalf("WorkDir = %q, want /worktrees/gated-run", got.GetWorkDir())
	}
	if got.GetExtraEnv()["CODEX_HOME"] != "/projected/home" || got.GetExtraEnv()["ACCOUNT_TOKEN"] != "secret" {
		t.Fatalf("ExtraEnv = %v, want managed account env", got.GetExtraEnv())
	}
	if got.GetHeadlessCapabilityProfile() != bossanovav1.HeadlessCapabilityProfile_HEADLESS_CAPABILITY_PROFILE_TRACKER_PLAN_ATTACHMENT_V1 {
		t.Fatalf("HeadlessCapabilityProfile = %s, want tracker-plan-attachment-v1", got.GetHeadlessCapabilityProfile())
	}
}

func TestPluginRunner_PreflightHeadlessCapabilityProfilePropagatesError(t *testing.T) {
	fc := &fakeAgentClient{preflightErr: errors.New("capability unavailable")}
	pr := NewPluginRunner(fc, NewTailer(zerolog.Nop()), t.TempDir(), zerolog.Nop())

	err := pr.PreflightHeadlessCapabilityProfile(
		context.Background(), t.TempDir(), "model", "", nil,
		bossanovav1.HeadlessCapabilityProfile_HEADLESS_CAPABILITY_PROFILE_TRACKER_PLAN_ATTACHMENT_V1,
	)
	if !errors.Is(err, fc.preflightErr) {
		t.Fatalf("PreflightHeadlessCapabilityProfile error = %v, want wrapped %v", err, fc.preflightErr)
	}
}

func TestPluginRunner_Start_PropagatesError(t *testing.T) {
	fc := &fakeAgentClient{startErr: errors.New("boom")}
	pr := NewPluginRunner(fc, NewTailer(zerolog.Nop()), t.TempDir(), zerolog.Nop())
	_, err := pr.Start(context.Background(), "/w", "p", nil, "sid", "", "", nil)
	if err == nil || !errors.Is(err, fc.startErr) && err.Error() != "boom" {
		t.Errorf("expected wrapped err, got %v", err)
	}
}
