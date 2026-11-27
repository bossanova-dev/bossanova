package server

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"

	"connectrpc.com/connect"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossd/internal/domain"
	"github.com/recurser/bossd/internal/setupcheck"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// SetupProbes holds host-local read-only probes for ListSetupChecks.
type SetupProbes struct {
	LookPath func(string) (string, error)
	GHAuth   func(context.Context) *pb.RepairDoctorCheck
	Skills   func(context.Context) setupcheck.SkillsFact
}

// ListSetupChecks evaluates every registered setup check for this machine and,
// when provided, one repository. It is read-only and gathers each source
// independently so an unreadable source reports BLOCKED rather than OK.
func (s *Server) ListSetupChecks(ctx context.Context, req *connect.Request[pb.ListSetupChecksRequest]) (*connect.Response[pb.ListSetupChecksResponse], error) {
	facts := setupcheck.Facts{
		GOOS: runtime.GOOS,
	}
	resolvedRepoID, _, err := s.gatherSetupCheckRepo(ctx, req.Msg, &facts)
	if err != nil {
		return nil, err
	}

	s.gatherSetupCheckRunners(&facts)
	facts.GHAuth = s.probeSetupGHAuth(ctx)
	facts.Node = s.probeSetupBinary("node")
	facts.Tmux = s.probeSetupBinary("tmux")
	facts.Skills = s.probeSetupSkills(ctx)

	results := setupcheck.Evaluate(facts)
	checks := make([]*pb.SetupCheck, 0, len(results))
	for _, r := range results {
		checks = append(checks, setupCheckResultToProto(r))
	}

	return connect.NewResponse(&pb.ListSetupChecksResponse{
		Checks:    checks,
		CheckedAt: timestamppb.Now(),
		RepoId:    resolvedRepoID,
	}), nil
}

func (s *Server) gatherSetupCheckRepo(ctx context.Context, req *pb.ListSetupChecksRequest, facts *setupcheck.Facts) (repoID, repoLocalPath string, err error) {
	if req.GetRepoId() != "" && req.GetWorkspacePath() != "" {
		return "", "", connect.NewError(connect.CodeInvalidArgument, errors.New("set at most one of repo_id or workspace_path"))
	}

	if req.GetRepoId() != "" {
		if s.repos == nil {
			facts.RepoUnreadable = "repository store unavailable"
			return "", "", nil
		}
		repo, getErr := s.repos.Get(ctx, req.GetRepoId())
		if errors.Is(getErr, domain.ErrNotFound) || (getErr == nil && repo == nil) {
			return "", "", connect.NewError(connect.CodeNotFound, errors.New("repository not found"))
		}
		if getErr != nil {
			facts.RepoUnreadable = getErr.Error()
			return "", "", nil
		}
		facts.RepoRegistered = true
		facts.RepoID = repo.ID
		facts.RepoLocalPath = repo.LocalPath
		return repo.ID, repo.LocalPath, nil
	}

	if req.GetWorkspacePath() == "" {
		facts.NoWorkspacePath = true
		return "", "", nil
	}

	absPath, absErr := filepath.Abs(req.GetWorkspacePath())
	if absErr != nil {
		return "", "", connect.NewError(connect.CodeInvalidArgument, errors.New("invalid workspace path"))
	}
	facts.WorkspacePath = absPath
	if s.repos == nil || s.sessions == nil {
		facts.RepoUnreadable = "repository store unavailable"
		return "", "", nil
	}
	repo, _, ownerErr := s.workspaceOwner(ctx, absPath)
	if ownerErr != nil {
		facts.RepoUnreadable = ownerErr.Error()
		return "", "", nil
	}
	if repo == nil {
		facts.RepoRegistered = false
		return "", "", nil
	}
	facts.RepoRegistered = true
	facts.RepoID = repo.ID
	facts.RepoLocalPath = repo.LocalPath
	return repo.ID, repo.LocalPath, nil
}

func (s *Server) gatherSetupCheckRunners(facts *setupcheck.Facts) {
	if s.pluginHost == nil {
		facts.PluginHostUnreadable = true
		return
	}
	runners := s.pluginHost.AgentClientNames()
	facts.AgentRunners = runners
	if len(runners) == 0 {
		return
	}
	facts.AgentCLIs = make(map[string]setupcheck.BinaryFact, len(runners))
	for _, runner := range runners {
		bin := setupcheck.AgentCLIBinary(runner)
		if _, done := facts.AgentCLIs[bin]; done {
			continue
		}
		facts.AgentCLIs[bin] = s.probeSetupBinary(bin)
	}
}

func (s *Server) probeSetupBinary(name string) setupcheck.BinaryFact {
	if s.setupProbes.LookPath == nil {
		return setupcheck.BinaryFact{
			State: setupcheck.BinaryUnreadable,
			Err:   fmt.Sprintf("PATH lookup unavailable for %s", name),
		}
	}
	path, err := s.setupProbes.LookPath(name)
	switch {
	case err == nil:
		return setupcheck.BinaryFact{State: setupcheck.BinaryFound, Path: path}
	case errors.Is(err, exec.ErrNotFound):
		return setupcheck.BinaryFact{State: setupcheck.BinaryMissing, Err: fmt.Sprintf("%s not found on bossd PATH", name)}
	default:
		return setupcheck.BinaryFact{State: setupcheck.BinaryUnreadable, Err: err.Error()}
	}
}

func (s *Server) probeSetupGHAuth(ctx context.Context) setupcheck.GHAuthFact {
	if s.setupProbes.GHAuth != nil {
		return classifySetupGHAuthCheck(s.setupProbes.GHAuth(ctx))
	}
	if s.setupProbes.LookPath == nil {
		return setupcheck.GHAuthFact{
			State:  setupcheck.GHAuthUnreadable,
			Detail: "gh probe unavailable",
		}
	}
	if _, err := s.setupProbes.LookPath("gh"); err != nil {
		if errors.Is(err, exec.ErrNotFound) {
			return setupcheck.GHAuthFact{
				State:  setupcheck.GHAuthNotInstalled,
				Detail: "gh CLI not found on bossd PATH",
			}
		}
		return setupcheck.GHAuthFact{
			State:  setupcheck.GHAuthUnreadable,
			Detail: fmt.Sprintf("look up gh on bossd PATH: %v", err),
		}
	}
	return classifySetupGHAuthCheck(githubAuthCheck(ctx))
}

func classifySetupGHAuthCheck(check *pb.RepairDoctorCheck) setupcheck.GHAuthFact {
	if check == nil {
		return setupcheck.GHAuthFact{
			State:  setupcheck.GHAuthUnreadable,
			Detail: "gh auth probe returned no result",
		}
	}
	detail := check.GetDetail()
	if check.GetOk() {
		return setupcheck.GHAuthFact{
			State:  setupcheck.GHAuthOK,
			Detail: detail,
		}
	}
	lower := strings.ToLower(detail)
	switch {
	case strings.Contains(lower, "executable file not found"):
		return setupcheck.GHAuthFact{
			State:  setupcheck.GHAuthNotInstalled,
			Detail: detail,
		}
	case strings.Contains(lower, "not authenticated") || strings.Contains(lower, "gh auth login"):
		return setupcheck.GHAuthFact{
			State:  setupcheck.GHAuthNotLoggedIn,
			Detail: detail,
		}
	default:
		return setupcheck.GHAuthFact{
			State:  setupcheck.GHAuthUnreadable,
			Detail: detail,
		}
	}
}

func (s *Server) probeSetupSkills(ctx context.Context) setupcheck.SkillsFact {
	if s.setupProbes.Skills == nil {
		return setupcheck.SkillsFact{
			State: setupcheck.SkillsUnreadable,
			Err:   "skill version probe unavailable",
		}
	}
	return s.setupProbes.Skills(ctx)
}

func setupCheckResultToProto(r setupcheck.Result) *pb.SetupCheck {
	out := &pb.SetupCheck{
		Id:             r.ID,
		Area:           setupCheckAreaToProto(r.Area),
		Status:         setupCheckStatusToProto(r.Status),
		Title:          r.Title,
		Evidence:       r.Evidence,
		Recommendation: r.Recommendation,
		AddedInVersion: r.AddedInVersion,
	}
	if r.Fix != nil {
		out.Fix = &pb.SetupCheckFix{
			Kind:      setupCheckFixKindToProto(r.Fix.Kind),
			Command:   r.Fix.Command,
			AgentTask: r.Fix.AgentTask,
		}
	}
	return out
}

func setupCheckAreaToProto(a setupcheck.Area) pb.SetupCheckArea {
	switch a {
	case setupcheck.AreaMachine:
		return pb.SetupCheckArea_SETUP_CHECK_AREA_MACHINE
	case setupcheck.AreaRepo:
		return pb.SetupCheckArea_SETUP_CHECK_AREA_REPO
	case setupcheck.AreaTracker:
		return pb.SetupCheckArea_SETUP_CHECK_AREA_TRACKER
	case setupcheck.AreaCI:
		return pb.SetupCheckArea_SETUP_CHECK_AREA_CI
	case setupcheck.AreaProof:
		return pb.SetupCheckArea_SETUP_CHECK_AREA_PROOF
	case setupcheck.AreaCrons:
		return pb.SetupCheckArea_SETUP_CHECK_AREA_CRONS
	case setupcheck.AreaCloudsAndDevices:
		return pb.SetupCheckArea_SETUP_CHECK_AREA_CLOUDS_AND_DEVICES
	default:
		return pb.SetupCheckArea_SETUP_CHECK_AREA_UNSPECIFIED
	}
}

func setupCheckStatusToProto(st setupcheck.Status) pb.SetupCheckStatus {
	switch st {
	case setupcheck.StatusOK:
		return pb.SetupCheckStatus_SETUP_CHECK_STATUS_OK
	case setupcheck.StatusMissing:
		return pb.SetupCheckStatus_SETUP_CHECK_STATUS_MISSING
	case setupcheck.StatusOutdated:
		return pb.SetupCheckStatus_SETUP_CHECK_STATUS_OUTDATED
	case setupcheck.StatusRecommended:
		return pb.SetupCheckStatus_SETUP_CHECK_STATUS_RECOMMENDED
	case setupcheck.StatusDeclined:
		return pb.SetupCheckStatus_SETUP_CHECK_STATUS_DECLINED
	case setupcheck.StatusBlocked:
		return pb.SetupCheckStatus_SETUP_CHECK_STATUS_BLOCKED
	default:
		return pb.SetupCheckStatus_SETUP_CHECK_STATUS_UNSPECIFIED
	}
}

func setupCheckFixKindToProto(k setupcheck.FixKind) pb.SetupCheckFixKind {
	switch k {
	case setupcheck.FixKindCommand:
		return pb.SetupCheckFixKind_SETUP_CHECK_FIX_KIND_COMMAND
	case setupcheck.FixKindAgentTask:
		return pb.SetupCheckFixKind_SETUP_CHECK_FIX_KIND_AGENT_TASK
	default:
		return pb.SetupCheckFixKind_SETUP_CHECK_FIX_KIND_UNSPECIFIED
	}
}
