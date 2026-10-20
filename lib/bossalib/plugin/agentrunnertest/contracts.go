package agentrunnertest

import (
	"context"

	v1 "github.com/recurser/bossalib/gen/bossanova/v1"
	"google.golang.org/protobuf/proto"
)

type contract struct {
	name string
	call func(context.Context, v1.AgentRunnerServiceClient, Env, bool) (proto.Message, error)
}

var contracts = []contract{
	{"GetInfo", func(ctx context.Context, c v1.AgentRunnerServiceClient, e Env, empty bool) (proto.Message, error) {
		q := &v1.AgentRunnerServiceGetInfoRequest{}
		return c.GetInfo(ctx, q)
	}},
	{"PreflightHeadlessRun", func(ctx context.Context, c v1.AgentRunnerServiceClient, e Env, empty bool) (proto.Message, error) {
		q := &v1.PreflightHeadlessRunRequest{}
		if !empty {
			*q = v1.PreflightHeadlessRunRequest{WorkDir: e.WorkDir}
		}
		return c.PreflightHeadlessRun(ctx, q)
	}},
	{"StartRun", func(ctx context.Context, c v1.AgentRunnerServiceClient, e Env, empty bool) (proto.Message, error) {
		return c.StartRun(ctx, &v1.StartAgentRunRequest{})
	}},
	{"StopRun", func(ctx context.Context, c v1.AgentRunnerServiceClient, e Env, empty bool) (proto.Message, error) {
		q := &v1.StopAgentRunRequest{}
		if !empty {
			*q = v1.StopAgentRunRequest{SessionId: e.UnknownSessionID}
		}
		return c.StopRun(ctx, q)
	}},
	{"IsRunning", func(ctx context.Context, c v1.AgentRunnerServiceClient, e Env, empty bool) (proto.Message, error) {
		q := &v1.IsAgentRunningRequest{}
		if !empty {
			*q = v1.IsAgentRunningRequest{SessionId: e.UnknownSessionID}
		}
		return c.IsRunning(ctx, q)
	}},
	{"ExitStatus", func(ctx context.Context, c v1.AgentRunnerServiceClient, e Env, empty bool) (proto.Message, error) {
		q := &v1.AgentExitStatusRequest{}
		if !empty {
			*q = v1.AgentExitStatusRequest{SessionId: e.UnknownSessionID}
		}
		return c.ExitStatus(ctx, q)
	}},
	{"ConfigureFinalizeHook", func(ctx context.Context, c v1.AgentRunnerServiceClient, e Env, empty bool) (proto.Message, error) {
		q := &v1.ConfigureFinalizeHookRequest{}
		if !empty {
			*q = v1.ConfigureFinalizeHookRequest{WorkDir: e.WorkDir, SessionId: e.UnknownSessionID, AgentSessionId: e.UnknownSessionID, HookToken: "conformance-token", HookPort: 12345}
		}
		return c.ConfigureFinalizeHook(ctx, q)
	}},
	{"RemoveAgentRunHook", func(ctx context.Context, c v1.AgentRunnerServiceClient, e Env, empty bool) (proto.Message, error) {
		q := &v1.RemoveAgentRunHookRequest{}
		if !empty {
			*q = v1.RemoveAgentRunHookRequest{WorkDir: e.WorkDir, AgentSessionId: e.UnknownSessionID}
		}
		return c.RemoveAgentRunHook(ctx, q)
	}},
	{"BuildInteractiveCommand", func(ctx context.Context, c v1.AgentRunnerServiceClient, e Env, empty bool) (proto.Message, error) {
		q := &v1.BuildInteractiveCommandRequest{}
		if !empty {
			*q = v1.BuildInteractiveCommandRequest{WorktreePath: e.WorkDir, SessionId: e.UnknownSessionID, LogPath: e.LogPath}
		}
		return c.BuildInteractiveCommand(ctx, q)
	}},
	{"ResolveInteractiveSessionID", func(ctx context.Context, c v1.AgentRunnerServiceClient, e Env, empty bool) (proto.Message, error) {
		q := &v1.ResolveInteractiveSessionIDRequest{}
		if !empty {
			*q = v1.ResolveInteractiveSessionIDRequest{WorkDir: e.WorkDir, RequestedSessionId: e.UnknownSessionID}
		}
		return c.ResolveInteractiveSessionID(ctx, q)
	}},
	{"ListIgnoredDirtyFiles", func(ctx context.Context, c v1.AgentRunnerServiceClient, e Env, empty bool) (proto.Message, error) {
		q := &v1.ListIgnoredDirtyFilesRequest{}
		if !empty {
			*q = v1.ListIgnoredDirtyFilesRequest{WorkDir: e.WorkDir}
		}
		return c.ListIgnoredDirtyFiles(ctx, q)
	}},
	{"GetChatTitle", func(ctx context.Context, c v1.AgentRunnerServiceClient, e Env, empty bool) (proto.Message, error) {
		q := &v1.GetChatTitleRequest{}
		if !empty {
			*q = v1.GetChatTitleRequest{WorkDir: e.WorkDir, SessionId: e.UnknownSessionID}
		}
		return c.GetChatTitle(ctx, q)
	}},
	{"SuggestPRTitle", func(ctx context.Context, c v1.AgentRunnerServiceClient, e Env, empty bool) (proto.Message, error) {
		q := &v1.SuggestPRTitleRequest{}
		if !empty {
			*q = v1.SuggestPRTitleRequest{WorkDir: e.WorkDir, CurrentTitle: "Conformance change", PrBody: "Check runner contracts", GitLog: "test: check contracts"}
		}
		return c.SuggestPRTitle(ctx, q)
	}},
	{"HasQuestionPrompt", func(ctx context.Context, c v1.AgentRunnerServiceClient, e Env, empty bool) (proto.Message, error) {
		q := &v1.HasQuestionPromptRequest{}
		return c.HasQuestionPrompt(ctx, q)
	}},
	{"HasWorkingIndicator", func(ctx context.Context, c v1.AgentRunnerServiceClient, e Env, empty bool) (proto.Message, error) {
		q := &v1.HasWorkingIndicatorRequest{}
		return c.HasWorkingIndicator(ctx, q)
	}},
	{"LastTurnIsUser", func(ctx context.Context, c v1.AgentRunnerServiceClient, e Env, empty bool) (proto.Message, error) {
		q := &v1.LastTurnIsUserRequest{}
		if !empty {
			*q = v1.LastTurnIsUserRequest{WorkDir: e.WorkDir, AgentSessionId: e.UnknownSessionID}
		}
		return c.LastTurnIsUser(ctx, q)
	}},
	{"DetectUsageLimit", func(ctx context.Context, c v1.AgentRunnerServiceClient, e Env, empty bool) (proto.Message, error) {
		q := &v1.DetectUsageLimitRequest{}
		return c.DetectUsageLimit(ctx, q)
	}},
	{"ProbeRateLimit", func(ctx context.Context, c v1.AgentRunnerServiceClient, e Env, empty bool) (proto.Message, error) {
		q := &v1.ProbeRateLimitRequest{}
		return c.ProbeRateLimit(ctx, q)
	}},
	{"TranscriptExists", func(ctx context.Context, c v1.AgentRunnerServiceClient, e Env, empty bool) (proto.Message, error) {
		q := &v1.TranscriptExistsRequest{}
		if !empty {
			*q = v1.TranscriptExistsRequest{WorkDir: e.WorkDir, AgentSessionId: e.UnknownSessionID}
		}
		return c.TranscriptExists(ctx, q)
	}},
	{"ReadTranscript", func(ctx context.Context, c v1.AgentRunnerServiceClient, e Env, empty bool) (proto.Message, error) {
		q := &v1.ReadTranscriptRequest{}
		if !empty {
			*q = v1.ReadTranscriptRequest{WorkDir: e.WorkDir, AgentSessionId: e.UnknownSessionID}
		}
		return c.ReadTranscript(ctx, q)
	}},
	{"ProbeProgressLiveness", func(ctx context.Context, c v1.AgentRunnerServiceClient, e Env, empty bool) (proto.Message, error) {
		q := &v1.ProbeProgressLivenessRequest{}
		if !empty {
			*q = v1.ProbeProgressLivenessRequest{WorkDir: e.WorkDir, AgentSessionId: e.UnknownSessionID}
		}
		return c.ProbeProgressLiveness(ctx, q)
	}},
	{"RotationCapability", func(ctx context.Context, c v1.AgentRunnerServiceClient, e Env, empty bool) (proto.Message, error) {
		q := &v1.RotationCapabilityRequest{}
		return c.RotationCapability(ctx, q)
	}},
	{"MaterializeAccount", func(ctx context.Context, c v1.AgentRunnerServiceClient, e Env, empty bool) (proto.Message, error) {
		q := &v1.MaterializeAccountRequest{}
		if !empty {
			*q = v1.MaterializeAccountRequest{CredentialBlob: []byte("conformance-dummy-credential")}
		}
		return c.MaterializeAccount(ctx, q)
	}},
	{"DescribeMCPSurface", func(ctx context.Context, c v1.AgentRunnerServiceClient, e Env, empty bool) (proto.Message, error) {
		q := &v1.DescribeMCPSurfaceRequest{}
		if !empty {
			*q = v1.DescribeMCPSurfaceRequest{WorkDir: e.WorkDir}
		}
		return c.DescribeMCPSurface(ctx, q)
	}},
}
