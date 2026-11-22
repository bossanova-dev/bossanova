package main

import (
	"context"
	"errors"
	"time"

	"github.com/rs/zerolog"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/recurser/bossalib/contract/plugin/agenterr"
	"github.com/recurser/bossalib/contract/plugin/agentruntime"
	"github.com/recurser/bossalib/contract/plugin/plugin/hostclient"
	bossanovav1 "github.com/recurser/bossalib/gen/bossanova/v1"
)

const pluginName = "antigravity"
const pluginVersion = "1"

// Server implements AgentRunnerService for agy.
type Server struct {
	bossanovav1.UnimplementedAgentRunnerServiceServer
	host   hostclient.Client
	logger zerolog.Logger
	runner *Runner
	store  agyStore
	procs  processFiles
}

func newServer(host hostclient.Client, logger zerolog.Logger, runnerOpts ...Option) *Server {
	return &Server{
		host:   host,
		logger: logger,
		runner: NewRunner(logger, runnerOpts...),
		store:  newAgyStore(),
		procs:  osProcessFiles{},
	}
}

func (s *Server) GetInfo(_ context.Context, _ *bossanovav1.AgentRunnerServiceGetInfoRequest) (*bossanovav1.AgentRunnerServiceGetInfoResponse, error) {
	return &bossanovav1.AgentRunnerServiceGetInfoResponse{
		Info: &bossanovav1.PluginInfo{
			Name:         pluginName,
			Version:      pluginVersion,
			Capabilities: []string{"agent_runner"},
			UserSettings: []*bossanovav1.UserSetting{
				{
					Key:          "dangerously_skip_permissions",
					Label:        "Skip permission prompts",
					Description:  "Pass --dangerously-skip-permissions to agy. Use only in trusted worktrees.",
					Type:         bossanovav1.UserSettingType_USER_SETTING_TYPE_BOOL,
					DefaultValue: "false",
				},
				{
					Key:           "mode",
					Label:         "Mode",
					Description:   "agy --mode for every launch. Empty uses agy's default.",
					Type:          bossanovav1.UserSettingType_USER_SETTING_TYPE_ENUM,
					AllowedValues: append([]string{""}, agyModes...),
					DefaultValue:  "",
				},
				{
					Key:          "model",
					Label:        "Model",
					Description:  "Fallback agy --model for runs without their own (see `agy models`). Empty uses the agy default.",
					Type:         bossanovav1.UserSettingType_USER_SETTING_TYPE_STRING,
					DefaultValue: "",
				},
			},
		},
	}, nil
}

// StartAgentRun detaches the run from the RPC context, which gRPC cancels on
// return and would otherwise SIGTERM the new process.
func (s *Server) StartAgentRun(_ context.Context, req *bossanovav1.StartAgentRunRequest) (*bossanovav1.StartAgentRunResponse, error) {
	if err := requireTrustedProject(req.GetProjectTrust()); err != nil {
		return nil, err
	}
	sid, err := s.runner.Start(context.Background(), req.GetWorkDir(), req.GetPlan(), req.ResumeId, req.GetSessionId(), req.GetLogPath(), req.GetModel(), req.GetEffort(), req.GetExtraEnv())
	if errors.Is(err, errConversationNotResumed) {
		return nil, status.Errorf(codes.FailedPrecondition, "start run: %v", err)
	}
	if err != nil {
		return nil, status.Errorf(codes.Internal, "start run: %v", err)
	}
	return &bossanovav1.StartAgentRunResponse{SessionId: sid}, nil
}

func (s *Server) StopRun(_ context.Context, req *bossanovav1.StopAgentRunRequest) (*bossanovav1.StopAgentRunResponse, error) {
	if err := s.runner.stop(req.GetSessionId()); err != nil {
		return nil, status.Errorf(codes.NotFound, "stop run: %v", err)
	}
	return &bossanovav1.StopAgentRunResponse{}, nil
}

func (s *Server) IsRunning(_ context.Context, req *bossanovav1.IsAgentRunningRequest) (*bossanovav1.IsAgentRunningResponse, error) {
	return &bossanovav1.IsAgentRunningResponse{Running: s.runner.isRunning(req.GetSessionId())}, nil
}

func (s *Server) ExitStatus(_ context.Context, req *bossanovav1.AgentExitStatusRequest) (*bossanovav1.AgentExitStatusResponse, error) {
	if s.runner.isRunning(req.GetSessionId()) {
		return &bossanovav1.AgentExitStatusResponse{IsComplete: false}, nil
	}
	err := s.runner.exitError(req.GetSessionId())
	resp := &bossanovav1.AgentExitStatusResponse{IsComplete: true}
	if err == nil {
		return resp, nil
	}
	resp.ExitError = err.Error()
	var ul agenterr.ErrUsageLimited
	var fc string
	switch {
	case errors.Is(err, ErrAuthRequired):
		fc = agenterr.KindAuthInvalidated.String()
	case errors.As(err, &ul):
		fc = agenterr.KindUsageExhausted.String()
		if !ul.ResetAt.IsZero() {
			resp.ResetAt = timestamppb.New(ul.ResetAt)
		}
	case errors.As(err, new(agenterr.ErrRateLimited)):
		fc = agenterr.KindRateLimited.String()
	default:
		return resp, nil
	}
	resp.FailureClass = &fc
	return resp, nil
}

// ConfigureFinalizeHook reports unsupported, so bossd polls ExitStatus. agy
// reads hooks only from <workspace>/.agents/hooks.json or
// ~/.gemini/config/hooks.json, and `--settings` rejects a "hooks" key, so a
// run-scoped hook would mean writing into the repository's own .agents
// directory.
func (s *Server) ConfigureFinalizeHook(_ context.Context, _ *bossanovav1.ConfigureFinalizeHookRequest) (*bossanovav1.ConfigureFinalizeHookResponse, error) {
	return &bossanovav1.ConfigureFinalizeHookResponse{IsSupported: false}, nil
}

func (s *Server) RemoveAgentRunHook(_ context.Context, _ *bossanovav1.RemoveAgentRunHookRequest) (*bossanovav1.RemoveAgentRunHookResponse, error) {
	return &bossanovav1.RemoveAgentRunHookResponse{IsSupported: false}, nil
}

func (s *Server) BuildInteractiveCommand(_ context.Context, req *bossanovav1.BuildInteractiveCommandRequest) (*bossanovav1.BuildInteractiveCommandResponse, error) {
	if err := requireTrustedProject(req.GetProjectTrust()); err != nil {
		return nil, err
	}
	return s.runner.buildInteractiveCommand(req)
}

func (s *Server) ResolveInteractiveSessionID(_ context.Context, req *bossanovav1.ResolveInteractiveSessionIDRequest) (*bossanovav1.ResolveInteractiveSessionIDResponse, error) {
	return resolveInteractiveSession(s.store, s.procs, req), nil
}

// ListIgnoredDirtyFiles is empty: the plugin writes nothing into a worktree.
func (s *Server) ListIgnoredDirtyFiles(_ context.Context, _ *bossanovav1.ListIgnoredDirtyFilesRequest) (*bossanovav1.ListIgnoredDirtyFilesResponse, error) {
	return &bossanovav1.ListIgnoredDirtyFilesResponse{Paths: []string{}}, nil
}

// GetChatTitle prefers agy's stored conversation title and falls back to the
// first user request. Explicit stays false: agy's title is generated, and a
// rename is indistinguishable from it.
func (s *Server) GetChatTitle(_ context.Context, req *bossanovav1.GetChatTitleRequest) (*bossanovav1.GetChatTitleResponse, error) {
	resp := &bossanovav1.GetChatTitleResponse{Supported: true}
	path, ok := s.store.transcriptPath(req.GetSessionId())
	if !ok {
		return resp, nil
	}
	if title := s.store.conversationTitle(req.GetSessionId()); title != "" {
		resp.Title = chatTitleFromRequest(title)
		return resp, nil
	}
	if steps, err := readTranscriptSteps(path); err == nil {
		resp.Title = chatTitleFromRequest(firstUserRequest(steps))
	}
	return resp, nil
}

// SuggestPRTitle has no opinion for an untrusted repository, because the
// one-shot would load the repository's .agents configuration.
func (s *Server) SuggestPRTitle(ctx context.Context, req *bossanovav1.SuggestPRTitleRequest) (*bossanovav1.SuggestPRTitleResponse, error) {
	if !req.GetShouldTrustProjectConfig() {
		return &bossanovav1.SuggestPRTitleResponse{Supported: false}, nil
	}
	title, err := s.runner.suggestPRTitle(ctx, req)
	if err != nil {
		s.logger.Warn().Err(err).Msg("antigravity SuggestPRTitle failed; daemon will fall back to its deterministic title")
		return &bossanovav1.SuggestPRTitleResponse{Supported: false}, nil
	}
	if title == "" {
		return &bossanovav1.SuggestPRTitleResponse{Supported: false}, nil
	}
	return &bossanovav1.SuggestPRTitleResponse{Supported: true, Title: title}, nil
}

// HasQuestionPrompt reports agy's approval menu as a question, and its folder
// trust modal as blocking input without a question.
func (s *Server) HasQuestionPrompt(_ context.Context, req *bossanovav1.HasQuestionPromptRequest) (*bossanovav1.HasQuestionPromptResponse, error) {
	st := parsePane(req.GetPaneContent())
	return &bossanovav1.HasQuestionPromptResponse{
		HasPrompt:   st.question,
		BlocksInput: st.question || st.trustModal,
	}, nil
}

func (s *Server) HasWorkingIndicator(_ context.Context, req *bossanovav1.HasWorkingIndicatorRequest) (*bossanovav1.HasWorkingIndicatorResponse, error) {
	return &bossanovav1.HasWorkingIndicatorResponse{IsWorking: parsePane(req.GetPaneContent()).working}, nil
}

func (s *Server) LastTurnIsUser(_ context.Context, req *bossanovav1.LastTurnIsUserRequest) (*bossanovav1.LastTurnIsUserResponse, error) {
	path, ok := s.store.transcriptPath(req.GetAgentSessionId())
	if !ok {
		return &bossanovav1.LastTurnIsUserResponse{}, nil
	}
	steps, err := readTranscriptSteps(path)
	if err != nil {
		return &bossanovav1.LastTurnIsUserResponse{}, nil
	}
	return &bossanovav1.LastTurnIsUserResponse{IsUser: lastTurnIsUser(steps)}, nil
}

func (s *Server) DetectUsageLimit(_ context.Context, req *bossanovav1.DetectUsageLimitRequest) (*bossanovav1.DetectUsageLimitResponse, error) {
	limited, resetAt := usageBanner(req.GetPaneContent(), time.Now())
	resp := &bossanovav1.DetectUsageLimitResponse{Limited: limited}
	if resetAt != nil {
		resp.ResetAt = timestamppb.New(*resetAt)
	}
	return resp, nil
}

// ProbeRateLimit reads agy's local /usage report for the account selected by
// credential_env. An empty overlay names no account, and any failure reports
// UNSUPPORTED with limited=false so a broken probe never blocks work.
func (s *Server) ProbeRateLimit(ctx context.Context, req *bossanovav1.ProbeRateLimitRequest) (*bossanovav1.ProbeRateLimitResponse, error) {
	unsupported := &bossanovav1.ProbeRateLimitResponse{Status: &bossanovav1.RateLimitStatus{
		Status: bossanovav1.RateLimitPlanStatus_RATE_LIMIT_PLAN_STATUS_UNSUPPORTED,
	}}
	if len(req.GetCredentialEnv()) == 0 {
		return unsupported, nil
	}
	st, err := s.runner.probeUsage(ctx, req.GetCredentialEnv())
	if err != nil {
		s.logger.Warn().Err(err).Msg("antigravity usage probe failed")
		return unsupported, nil
	}
	return &bossanovav1.ProbeRateLimitResponse{Status: st}, nil
}

func (s *Server) TranscriptExists(_ context.Context, req *bossanovav1.TranscriptExistsRequest) (*bossanovav1.TranscriptExistsResponse, error) {
	path, ok := s.store.transcriptPath(req.GetAgentSessionId())
	return &bossanovav1.TranscriptExistsResponse{Exists: ok && agentruntime.NonEmptyFile(path)}, nil
}

func (s *Server) ReadTranscript(_ context.Context, req *bossanovav1.ReadTranscriptRequest) (*bossanovav1.ReadTranscriptResponse, error) {
	path, ok := s.store.transcriptPath(req.GetAgentSessionId())
	if !ok || !agentruntime.NonEmptyFile(path) {
		return &bossanovav1.ReadTranscriptResponse{}, nil
	}
	steps, err := readTranscriptSteps(path)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "read transcript: %v", err)
	}
	msgs, final := transcriptMessages(steps, int(req.GetMaxMessages()))
	return &bossanovav1.ReadTranscriptResponse{Messages: msgs, FinalAssistantText: final, Exists: true}, nil
}

func (s *Server) ProbeProgressLiveness(_ context.Context, req *bossanovav1.ProbeProgressLivenessRequest) (*bossanovav1.ProbeProgressLivenessResponse, error) {
	unknown := &bossanovav1.ProbeProgressLivenessResponse{Phase: bossanovav1.AgentProgressPhase_AGENT_PROGRESS_PHASE_UNKNOWN}
	path, ok := s.store.transcriptPath(req.GetAgentSessionId())
	if !ok {
		return unknown, nil
	}
	snap, ok := readProgress(path)
	if !ok {
		return unknown, nil
	}
	resp := &bossanovav1.ProbeProgressLivenessResponse{
		LastProgressAt: timestamppb.New(snap.lastProgressAt),
		Phase:          snap.phase,
		IsKnown:        true,
	}
	if !snap.lastAssistantMessageAt.IsZero() {
		resp.LastAssistantMessageAt = timestamppb.New(snap.lastAssistantMessageAt)
	}
	return resp, nil
}

func (s *Server) RotationCapability(_ context.Context, _ *bossanovav1.RotationCapabilityRequest) (*bossanovav1.RotationCapabilityResponse, error) {
	return &bossanovav1.RotationCapabilityResponse{
		SupportsRotation: true,
		AuthKind:         bossanovav1.AuthKind_AUTH_KIND_HOME_DIR,
	}, nil
}

// MaterializeAgentAccount turns the stored antigravity OAuth token JSON into a
// per-account HOME file spec (mode 0600) plus SSH_CLIENT=1 so agy bypasses the
// OS keyring and reads its file-based token store under HOME. The blob is NEVER
// logged.
func (s *Server) MaterializeAgentAccount(_ context.Context, req *bossanovav1.MaterializeAgentAccountRequest) (*bossanovav1.MaterializeAgentAccountResponse, error) {
	return &bossanovav1.MaterializeAgentAccountResponse{
		Env: map[string]string{
			"SSH_CLIENT": "1",
		},
		Files: []*bossanovav1.MaterializedFile{{
			RelativePath: ".gemini/antigravity-cli/antigravity-oauth-token",
			Content:      req.GetCredentialBlob(),
			Mode:         0o600,
		}},
		HomeDirEnvKey: "HOME",
	}, nil
}
