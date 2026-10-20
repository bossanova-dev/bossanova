package agentrunnertest

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	v1 "github.com/recurser/bossalib/gen/bossanova/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func fullMethod(name string) string {
	return "/" + v1.AgentRunnerService_ServiceDesc.ServiceName + "/" + name
}

func unsupportedNames(r Runner) []Violation {
	var out []Violation
	for _, name := range r.Unsupported {
		known := slices.ContainsFunc(v1.AgentRunnerService_ServiceDesc.Methods, func(m grpc.MethodDesc) bool { return fullMethod(m.MethodName) == name })
		if !known {
			out = append(out, Violation{Method: name, Problem: "Unsupported name is not an AgentRunnerService method"})
		}
	}
	return out
}

func checkMethod(parent context.Context, r Runner, e Env, row contract) []Violation {
	ctx, cancel := context.WithTimeout(parent, 15*time.Second)
	defer cancel()
	var out []Violation
	fail := func(problem string) { out = append(out, Violation{Method: row.name, Problem: problem}) }
	if slices.Contains(r.Unsupported, fullMethod(row.name)) {
		_, err := row.call(ctx, r.Client, e, true)
		if status.Code(err) != codes.Unimplemented {
			fail("declared unsupported but implemented: drop it from Unsupported")
		}
		return out
	}
	if row.name == "StartAgentRun" {
		return checkLifecycle(ctx, r, e)
	}
	var before map[string]bool
	if row.name == "ConfigureFinalizeHook" {
		var err error
		before, err = filesIn(e.WorkDir)
		if err != nil {
			fail("cannot inspect work directory before configuring hook")
			return out
		}
	}
	if row.name == "RemoveAgentRunHook" && !slices.Contains(r.Unsupported, fullMethod("ConfigureFinalizeHook")) {
		_, err := r.Client.ConfigureFinalizeHook(ctx, &v1.ConfigureFinalizeHookRequest{WorkDir: e.WorkDir, SessionId: e.UnknownSessionID, AgentSessionId: e.UnknownSessionID, HookToken: "conformance-token", HookPort: 12345})
		if err != nil {
			fail("cannot configure hook for removal check")
			return out
		}
	}
	response, err := row.call(ctx, r.Client, e, false)
	if err != nil {
		if status.Code(err) == codes.Unimplemented {
			fail("supported RPC returns Unimplemented")
		} else if row.name != "SuggestPRTitle" && (row.name != "StopRun" || status.Code(err) != codes.NotFound) {
			fail("RPC failed with " + status.Code(err).String())
		}
		return out
	}
	switch v := response.(type) {
	case *v1.AgentRunnerServiceGetInfoResponse:
		info := v.GetInfo()
		if info == nil || info.GetName() == "" || info.GetVersion() == "" || !slices.Contains(info.GetCapabilities(), "agent_runner") {
			fail("info must include name, version and agent_runner capability")
		}
	case *v1.IsAgentRunningResponse:
		if v.GetRunning() {
			fail("unknown session is running")
		}
	case *v1.AgentExitStatusResponse:
		if !v.GetIsComplete() {
			fail("unknown session must be complete")
		}
	case *v1.ConfigureFinalizeHookResponse:
		after, walkErr := filesIn(e.WorkDir)
		if walkErr != nil {
			fail("cannot inspect work directory after configuring hook")
			break
		}
		ignored, ignoredErr := r.Client.ListIgnoredDirtyFiles(ctx, &v1.ListIgnoredDirtyFilesRequest{WorkDir: e.WorkDir})
		if ignoredErr != nil {
			fail("cannot list hook-owned files")
			break
		}
		for path := range after {
			if !before[path] && !slices.Contains(ignored.GetPaths(), path) {
				fail("created hook file is absent from ListIgnoredDirtyFiles: " + path)
			}
		}
	case *v1.RemoveAgentRunHookResponse:
		if _, err = r.Client.RemoveAgentRunHook(ctx, &v1.RemoveAgentRunHookRequest{WorkDir: e.WorkDir, AgentSessionId: e.UnknownSessionID}); err != nil {
			fail("second hook removal failed")
		}
		fresh, mkdirErr := os.MkdirTemp(e.WorkDir, "conformance-empty-")
		if mkdirErr != nil {
			fail("cannot create fresh directory for absent hook check")
			break
		}
		defer func() { _ = os.RemoveAll(fresh) }()
		if _, err = r.Client.RemoveAgentRunHook(ctx, &v1.RemoveAgentRunHookRequest{WorkDir: fresh, AgentSessionId: e.UnknownSessionID}); err != nil {
			fail("absent hook removal failed")
		}
	case *v1.BuildInteractiveCommandResponse:
		if len(v.GetArgv()) == 0 || v.GetArgv()[0] == "" {
			fail("argv must have a non-empty executable")
		}
	case *v1.ResolveInteractiveSessionIDResponse:
		if v.GetFound() && v.GetSessionId() == "" {
			fail("found session must have a session_id")
		}
	case *v1.ListIgnoredDirtyFilesResponse:
		for _, path := range v.GetPaths() {
			if !relativeClean(path) {
				fail("ignored path must be relative, clean and contained in work_dir: " + path)
			}
		}
	case *v1.GetChatTitleResponse:
		if !v.GetSupported() && v.GetTitle() != "" {
			fail("unsupported title must be empty")
		}
	case *v1.SuggestPRTitleResponse:
		if v.GetSupported() && v.GetTitle() == "" {
			fail("supported title must be non-empty")
		}
	case *v1.HasQuestionPromptResponse:
		if v.GetHasPrompt() {
			fail("empty pane has a question prompt")
		}
		plain, plainErr := r.Client.HasQuestionPrompt(ctx, &v1.HasQuestionPromptRequest{PaneContent: []byte("A plain text pane.")})
		if plainErr != nil || plain.GetHasPrompt() {
			fail("plain text pane must return has_prompt=false")
		}
	case *v1.HasWorkingIndicatorResponse:
		if v.GetIsWorking() {
			fail("empty pane has a working indicator")
		}
		plain, plainErr := r.Client.HasWorkingIndicator(ctx, &v1.HasWorkingIndicatorRequest{PaneContent: []byte("A plain text pane.")})
		if plainErr != nil || plain.GetIsWorking() {
			fail("plain text pane must return is_working=false")
		}
	case *v1.LastTurnIsUserResponse:
		if v.GetIsUser() {
			fail("unknown session must return is_user=false")
		}
	case *v1.DetectUsageLimitResponse:
		if v.GetLimited() || v.GetResetAt() != nil {
			fail("empty pane must have no usage limit or reset_at")
		}
		plain, plainErr := r.Client.DetectUsageLimit(ctx, &v1.DetectUsageLimitRequest{PaneContent: []byte("A plain text pane.")})
		if plainErr != nil || plain.GetLimited() || plain.GetResetAt() != nil {
			fail("plain text pane must have no usage limit or reset_at")
		}
	case *v1.ProbeRateLimitResponse:
		if v.GetStatus() == nil || v.GetStatus().GetLimited() || v.GetStatus().GetStatus() != v1.RateLimitPlanStatus_RATE_LIMIT_PLAN_STATUS_UNSUPPORTED {
			fail("empty credential_env must return status UNSUPPORTED and limited=false")
		}
	case *v1.TranscriptExistsResponse:
		if v.GetExists() {
			fail("unknown session transcript must not exist")
		}
	case *v1.ReadTranscriptResponse:
		if v.GetExists() || len(v.GetMessages()) != 0 {
			fail("unknown session must have no transcript or messages")
		}
	case *v1.ProbeProgressLivenessResponse:
		if v.GetIsKnown() || v.GetLastProgressAt() != nil || v.GetPhase() != v1.AgentProgressPhase_AGENT_PROGRESS_PHASE_UNKNOWN {
			fail("unknown session must return is_known=false, absent last_progress_at and UNKNOWN phase")
		}
	case *v1.RotationCapabilityResponse:
		if v.GetSupportsRotation() {
			if v.GetAuthKind() != v1.AuthKind_AUTH_KIND_ENV_TOKEN && v.GetAuthKind() != v1.AuthKind_AUTH_KIND_HOME_DIR {
				fail("rotation support requires ENV_TOKEN or HOME_DIR auth_kind")
			}
		} else if v.GetAuthKind() != v1.AuthKind_AUTH_KIND_UNSPECIFIED && v.GetAuthKind() != v1.AuthKind_AUTH_KIND_NONE {
			fail("no rotation support requires UNSPECIFIED or NONE auth_kind")
		}
	case *v1.MaterializeAgentAccountResponse:
		capability, capErr := r.Client.RotationCapability(ctx, &v1.RotationCapabilityRequest{})
		if capErr != nil {
			fail("cannot cross-check RotationCapability")
			break
		}
		if !capability.GetSupportsRotation() {
			if len(v.GetEnv()) != 0 || len(v.GetFiles()) != 0 {
				fail("no rotation support must return no env or files")
			}
			break
		}
		switch capability.GetAuthKind() {
		case v1.AuthKind_AUTH_KIND_UNSPECIFIED, v1.AuthKind_AUTH_KIND_NONE:
			fail("rotation support requires ENV_TOKEN or HOME_DIR auth")
		case v1.AuthKind_AUTH_KIND_ENV_TOKEN:
			if len(v.GetEnv()) == 0 || v.GetHomeDirEnvKey() != "" {
				fail("ENV_TOKEN requires non-empty env and no home_dir_env_key")
			}
		case v1.AuthKind_AUTH_KIND_HOME_DIR:
			if len(v.GetFiles()) == 0 || v.GetHomeDirEnvKey() == "" {
				fail("HOME_DIR requires files and home_dir_env_key")
			}
			for _, file := range v.GetFiles() {
				if !relativeClean(file.GetRelativePath()) {
					fail("materialized file relative_path must be relative, clean and contained")
				}
			}
		}
	case *v1.DescribeMCPSurfaceResponse:
		if v.GetProbeError() != "" && len(v.GetServers()) != 0 {
			fail("probe_error requires empty servers")
		}
	}
	return out
}

func relativeClean(path string) bool {
	return path != "" && !filepath.IsAbs(path) && filepath.Clean(path) == path && !strings.HasPrefix(path, "..")
}

func filesIn(root string) (map[string]bool, error) {
	out := make(map[string]bool)
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return nil
		}
		relative, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		out[relative] = true
		return nil
	})
	return out, err
}

func checkLifecycle(parent context.Context, r Runner, e Env) []Violation {
	var out []Violation
	fail := func(problem string) { out = append(out, Violation{Method: "StartAgentRun", Problem: problem}) }
	request := &v1.StartAgentRunRequest{WorkDir: e.WorkDir, LogPath: e.LogPath, SessionId: "conformance-run", Plan: "Conformance"}
	if r.StartAgentRun != nil {
		request = r.StartAgentRun(e)
	}
	if request == nil {
		fail("Runner.StartAgentRun returned no request")
		return out
	}
	response, err := r.Client.StartAgentRun(parent, request)
	if err != nil {
		if status.Code(err) == codes.Unimplemented {
			fail("supported RPC returns Unimplemented")
		} else {
			fail("start failed with " + status.Code(err).String())
		}
		return out
	}
	id := response.GetSessionId()
	if id == "" {
		fail("session_id must be non-empty")
		return out
	}
	if _, err = r.Client.StopRun(parent, &v1.StopAgentRunRequest{SessionId: id}); err != nil && status.Code(err) != codes.NotFound {
		fail("stop failed with " + status.Code(err).String())
		return out
	}
	ctx, cancel := context.WithTimeout(parent, 10*time.Second)
	defer cancel()
	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()
	for {
		state, pollErr := r.Client.ExitStatus(ctx, &v1.AgentExitStatusRequest{SessionId: id})
		if pollErr != nil {
			fail("exit status failed with " + status.Code(pollErr).String())
			return out
		}
		if state.GetIsComplete() {
			break
		}
		select {
		case <-ctx.Done():
			fail(fmt.Sprintf("run did not complete within bounded poll (last is_complete=%t)", state.GetIsComplete()))
			return out
		case <-ticker.C:
		}
	}
	running, err := r.Client.IsRunning(parent, &v1.IsAgentRunningRequest{SessionId: id})
	if err != nil {
		fail("final IsRunning failed with " + status.Code(err).String())
	} else if running.GetRunning() {
		fail("completed run must not be running")
	}
	return out
}
