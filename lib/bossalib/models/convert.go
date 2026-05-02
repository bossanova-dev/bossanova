package models

import (
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/machine"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// --- Repo conversion ---

// RepoToProto converts a Repo to its protobuf representation.
func RepoToProto(r *Repo) *pb.Repo {
	p := &pb.Repo{
		Id:                r.ID,
		DisplayName:       r.DisplayName,
		LocalPath:         r.LocalPath,
		OriginUrl:         r.OriginURL,
		DefaultBaseBranch: r.DefaultBaseBranch,
		WorktreeBaseDir:   r.WorktreeBaseDir,
		SetupScript:       r.SetupScript,
		CreatedAt:         timestamppb.New(r.CreatedAt),
		UpdatedAt:         timestamppb.New(r.UpdatedAt),
	}
	return p
}

// RepoFromProto converts a protobuf Repo to the domain type.
func RepoFromProto(p *pb.Repo) *Repo {
	r := &Repo{
		ID:                p.Id,
		DisplayName:       p.DisplayName,
		LocalPath:         p.LocalPath,
		OriginURL:         p.OriginUrl,
		DefaultBaseBranch: p.DefaultBaseBranch,
		WorktreeBaseDir:   p.WorktreeBaseDir,
		SetupScript:       p.SetupScript,
		CreatedAt:         p.CreatedAt.AsTime(),
		UpdatedAt:         p.UpdatedAt.AsTime(),
	}
	return r
}

// --- Session conversion ---

// SessionToProto converts a Session to its protobuf representation.
func SessionToProto(s *Session) *pb.Session {
	p := &pb.Session{
		Id:                s.ID,
		RepoId:            s.RepoID,
		Title:             s.Title,
		Plan:              s.Plan,
		WorktreePath:      s.WorktreePath,
		BranchName:        s.BranchName,
		BaseBranch:        s.BaseBranch,
		State:             stateToProto(s.State),
		ClaudeSessionId:   s.ClaudeSessionID,
		PrNumber:          intPtrToInt32Ptr(s.PRNumber),
		PrUrl:             s.PRURL,
		LastCheckState:    checkStateToProto(s.LastCheckState),
		AutomationEnabled: s.AutomationEnabled,
		AttemptCount:      int32(s.AttemptCount),
		BlockedReason:     s.BlockedReason,
		CreatedAt:         timestamppb.New(s.CreatedAt),
		UpdatedAt:         timestamppb.New(s.UpdatedAt),
		DisplayLabel:      s.DisplayLabel,
		DisplayIntent:     pb.DisplayIntent(s.DisplayIntent),
		DisplaySpinner:    s.DisplaySpinner,
	}
	if s.ArchivedAt != nil {
		p.ArchivedAt = timestamppb.New(*s.ArchivedAt)
	}
	return p
}

// SessionFromProto converts a protobuf Session to the domain type.
func SessionFromProto(p *pb.Session) *Session {
	s := &Session{
		ID:                p.Id,
		RepoID:            p.RepoId,
		Title:             p.Title,
		Plan:              p.Plan,
		WorktreePath:      p.WorktreePath,
		BranchName:        p.BranchName,
		BaseBranch:        p.BaseBranch,
		State:             stateFromProto(p.State),
		ClaudeSessionID:   p.ClaudeSessionId,
		PRNumber:          int32PtrToIntPtr(p.PrNumber),
		PRURL:             p.PrUrl,
		LastCheckState:    checkStateFromProto(p.LastCheckState),
		AutomationEnabled: p.AutomationEnabled,
		AttemptCount:      int(p.AttemptCount),
		BlockedReason:     p.BlockedReason,
		CreatedAt:         p.CreatedAt.AsTime(),
		UpdatedAt:         p.UpdatedAt.AsTime(),
		DisplayLabel:      p.DisplayLabel,
		DisplayIntent:     int32(p.DisplayIntent),
		DisplaySpinner:    p.DisplaySpinner,
	}
	if p.ArchivedAt != nil {
		t := p.ArchivedAt.AsTime()
		s.ArchivedAt = &t
	}
	return s
}

// --- Attempt conversion ---

// AttemptToProto converts an Attempt to its protobuf representation.
func AttemptToProto(a *Attempt) *pb.Attempt {
	p := &pb.Attempt{
		Id:        a.ID,
		SessionId: a.SessionID,
		Trigger:   attemptTriggerToProto(a.Trigger),
		Result:    attemptResultToProto(a.Result),
		Error:     a.Error,
		CreatedAt: timestamppb.New(a.CreatedAt),
		UpdatedAt: timestamppb.New(a.UpdatedAt),
	}
	return p
}

// AttemptFromProto converts a protobuf Attempt to the domain type.
func AttemptFromProto(p *pb.Attempt) *Attempt {
	a := &Attempt{
		ID:        p.Id,
		SessionID: p.SessionId,
		Trigger:   attemptTriggerFromProto(p.Trigger),
		Result:    attemptResultFromProto(p.Result),
		Error:     p.Error,
		CreatedAt: p.CreatedAt.AsTime(),
		UpdatedAt: p.UpdatedAt.AsTime(),
	}
	return a
}

// --- State mapping ---

var stateToProtoMap = map[machine.State]pb.SessionState{
	machine.CreatingWorktree: pb.SessionState_SESSION_STATE_CREATING_WORKTREE,
	machine.StartingClaude:   pb.SessionState_SESSION_STATE_STARTING_CLAUDE,
	machine.PushingBranch:    pb.SessionState_SESSION_STATE_PUSHING_BRANCH,
	machine.OpeningDraftPR:   pb.SessionState_SESSION_STATE_OPENING_DRAFT_PR,
	machine.ImplementingPlan: pb.SessionState_SESSION_STATE_IMPLEMENTING_PLAN,
	machine.AwaitingChecks:   pb.SessionState_SESSION_STATE_AWAITING_CHECKS,
	machine.FixingChecks:     pb.SessionState_SESSION_STATE_FIXING_CHECKS,
	machine.GreenDraft:       pb.SessionState_SESSION_STATE_GREEN_DRAFT,
	machine.ReadyForReview:   pb.SessionState_SESSION_STATE_READY_FOR_REVIEW,
	machine.Blocked:          pb.SessionState_SESSION_STATE_BLOCKED,
	machine.Merged:           pb.SessionState_SESSION_STATE_MERGED,
	machine.Closed:           pb.SessionState_SESSION_STATE_CLOSED,
	machine.Finalizing:       pb.SessionState_SESSION_STATE_FINALIZING,
}

var stateFromProtoMap = map[pb.SessionState]machine.State{
	pb.SessionState_SESSION_STATE_CREATING_WORKTREE: machine.CreatingWorktree,
	pb.SessionState_SESSION_STATE_STARTING_CLAUDE:   machine.StartingClaude,
	pb.SessionState_SESSION_STATE_PUSHING_BRANCH:    machine.PushingBranch,
	pb.SessionState_SESSION_STATE_OPENING_DRAFT_PR:  machine.OpeningDraftPR,
	pb.SessionState_SESSION_STATE_IMPLEMENTING_PLAN: machine.ImplementingPlan,
	pb.SessionState_SESSION_STATE_AWAITING_CHECKS:   machine.AwaitingChecks,
	pb.SessionState_SESSION_STATE_FIXING_CHECKS:     machine.FixingChecks,
	pb.SessionState_SESSION_STATE_GREEN_DRAFT:       machine.GreenDraft,
	pb.SessionState_SESSION_STATE_READY_FOR_REVIEW:  machine.ReadyForReview,
	pb.SessionState_SESSION_STATE_BLOCKED:           machine.Blocked,
	pb.SessionState_SESSION_STATE_MERGED:            machine.Merged,
	pb.SessionState_SESSION_STATE_CLOSED:            machine.Closed,
	pb.SessionState_SESSION_STATE_FINALIZING:        machine.Finalizing,
}

func stateToProto(s machine.State) pb.SessionState {
	if v, ok := stateToProtoMap[s]; ok {
		return v
	}
	return pb.SessionState_SESSION_STATE_UNSPECIFIED
}

func stateFromProto(s pb.SessionState) machine.State {
	if v, ok := stateFromProtoMap[s]; ok {
		return v
	}
	return machine.CreatingWorktree
}

// --- CheckState mapping ---

func checkStateToProto(cs machine.CheckState) pb.ChecksOverall {
	switch cs {
	case machine.CheckStatePending:
		return pb.ChecksOverall_CHECKS_OVERALL_PENDING
	case machine.CheckStatePassed:
		return pb.ChecksOverall_CHECKS_OVERALL_PASSED
	case machine.CheckStateFailed:
		return pb.ChecksOverall_CHECKS_OVERALL_FAILED
	default:
		return pb.ChecksOverall_CHECKS_OVERALL_UNSPECIFIED
	}
}

func checkStateFromProto(cs pb.ChecksOverall) machine.CheckState {
	switch cs {
	case pb.ChecksOverall_CHECKS_OVERALL_PENDING:
		return machine.CheckStatePending
	case pb.ChecksOverall_CHECKS_OVERALL_PASSED:
		return machine.CheckStatePassed
	case pb.ChecksOverall_CHECKS_OVERALL_FAILED:
		return machine.CheckStateFailed
	default:
		return machine.CheckStateUnspecified
	}
}

// --- AttemptTrigger mapping ---

func attemptTriggerToProto(t AttemptTrigger) pb.AttemptTrigger {
	switch t {
	case AttemptTriggerCheckFailure:
		return pb.AttemptTrigger_ATTEMPT_TRIGGER_CHECK_FAILURE
	case AttemptTriggerConflict:
		return pb.AttemptTrigger_ATTEMPT_TRIGGER_CONFLICT
	case AttemptTriggerReviewFeedback:
		return pb.AttemptTrigger_ATTEMPT_TRIGGER_REVIEW_FEEDBACK
	default:
		return pb.AttemptTrigger_ATTEMPT_TRIGGER_UNSPECIFIED
	}
}

func attemptTriggerFromProto(t pb.AttemptTrigger) AttemptTrigger {
	switch t {
	case pb.AttemptTrigger_ATTEMPT_TRIGGER_CHECK_FAILURE:
		return AttemptTriggerCheckFailure
	case pb.AttemptTrigger_ATTEMPT_TRIGGER_CONFLICT:
		return AttemptTriggerConflict
	case pb.AttemptTrigger_ATTEMPT_TRIGGER_REVIEW_FEEDBACK:
		return AttemptTriggerReviewFeedback
	default:
		return AttemptTriggerUnspecified
	}
}

// --- AttemptResult mapping ---

func attemptResultToProto(r AttemptResult) pb.AttemptResult {
	switch r {
	case AttemptResultSuccess:
		return pb.AttemptResult_ATTEMPT_RESULT_SUCCESS
	case AttemptResultFailed:
		return pb.AttemptResult_ATTEMPT_RESULT_FAILED
	case AttemptResultIncomplete:
		return pb.AttemptResult_ATTEMPT_RESULT_INCOMPLETE
	default:
		return pb.AttemptResult_ATTEMPT_RESULT_UNSPECIFIED
	}
}

func attemptResultFromProto(r pb.AttemptResult) AttemptResult {
	switch r {
	case pb.AttemptResult_ATTEMPT_RESULT_SUCCESS:
		return AttemptResultSuccess
	case pb.AttemptResult_ATTEMPT_RESULT_FAILED:
		return AttemptResultFailed
	case pb.AttemptResult_ATTEMPT_RESULT_INCOMPLETE:
		return AttemptResultIncomplete
	default:
		return AttemptResultUnspecified
	}
}

// --- Helpers ---

func intPtrToInt32Ptr(v *int) *int32 {
	if v == nil {
		return nil
	}
	i := int32(*v)
	return &i
}

func int32PtrToIntPtr(v *int32) *int {
	if v == nil {
		return nil
	}
	i := int(*v)
	return &i
}
