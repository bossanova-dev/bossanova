// Package server implements the ConnectRPC DaemonService handler.
package server

import (
	"time"

	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/models"
	"github.com/recurser/bossalib/vcs"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// repoToProto converts a domain Repo to its protobuf representation.
func repoToProto(r *models.Repo) *pb.Repo {
	p := &pb.Repo{
		Id:                      r.ID,
		DisplayName:             r.DisplayName,
		LocalPath:               r.LocalPath,
		OriginUrl:               r.OriginURL,
		DefaultBaseBranch:       r.DefaultBaseBranch,
		WorktreeBaseDir:         r.WorktreeBaseDir,
		CanAutoMerge:            r.CanAutoMerge,
		CanAutoMergeDependabot:  r.CanAutoMergeDependabot,
		CanAutoAddressReviews:   r.CanAutoAddressReviews,
		CanAutoResolveConflicts: r.CanAutoResolveConflicts,
		MergeStrategy:           string(r.MergeStrategy),
		LinearApiKey:            r.LinearAPIKey,
		LinearTeamKey:           r.LinearTeamKey,
		CreatedAt:               timestamppb.New(r.CreatedAt),
		UpdatedAt:               timestamppb.New(r.UpdatedAt),
	}
	if r.SetupScript != nil {
		p.SetupScript = r.SetupScript
	}
	return p
}

// sessionToProto converts a domain Session to its protobuf representation.
func sessionToProto(s *models.Session) *pb.Session {
	p := &pb.Session{
		Id:                s.ID,
		RepoId:            s.RepoID,
		Title:             s.Title,
		Plan:              s.Plan,
		WorktreePath:      s.WorktreePath,
		BranchName:        s.BranchName,
		BaseBranch:        s.BaseBranch,
		State:             pb.SessionState(s.State),
		LastCheckState:    pb.ChecksOverall(s.LastCheckState),
		AutomationEnabled: s.AutomationEnabled,
		AttemptCount:      int32(s.AttemptCount),
		CreatedAt:         timestamppb.New(s.CreatedAt),
		UpdatedAt:         timestamppb.New(s.UpdatedAt),
	}
	if s.ClaudeSessionID != nil {
		p.ClaudeSessionId = s.ClaudeSessionID
	}
	if s.PRNumber != nil {
		n := int32(*s.PRNumber)
		p.PrNumber = &n
	}
	if s.PRURL != nil {
		p.PrUrl = s.PRURL
	}
	if s.BlockedReason != nil {
		p.BlockedReason = s.BlockedReason
	}
	if s.ArchivedAt != nil {
		p.ArchivedAt = timestamppb.New(*s.ArchivedAt)
	}
	return p
}

// claudeChatToProto converts a domain ClaudeChat to its protobuf representation.
func claudeChatToProto(c *models.ClaudeChat) *pb.ClaudeChat {
	return &pb.ClaudeChat{
		Id:        c.ID,
		SessionId: c.SessionID,
		ClaudeId:  c.ClaudeID,
		Title:     c.Title,
		DaemonId:  c.DaemonID,
		CreatedAt: timestamppb.New(c.CreatedAt),
	}
}

// constructPRURL is a package-local alias for vcs.ConstructPRURL.
func constructPRURL(originURL string, prNumber int) string {
	return vcs.ConstructPRURL(originURL, prNumber)
}

// attentionStatusToProto converts a vcs.AttentionStatus to its protobuf representation.
// Returns nil if the session does not need attention.
func attentionStatusToProto(a vcs.AttentionStatus) *pb.AttentionStatus {
	if !a.NeedsAttention {
		return nil
	}
	return &pb.AttentionStatus{
		NeedsAttention: true,
		Reason:         pb.AttentionReason(a.Reason),
		Summary:        a.Summary,
		Since:          timestamppb.New(a.Since),
	}
}

// autopilotWorkflowToProto converts a domain Workflow to its daemon-facing protobuf representation.
func autopilotWorkflowToProto(w *models.Workflow) *pb.AutopilotWorkflow {
	p := &pb.AutopilotWorkflow{
		Id:          w.ID,
		Status:      workflowStatusToProto(w.Status),
		CurrentStep: workflowStepToProto(w.CurrentStep),
		FlightLeg:   int32(w.FlightLeg),
		MaxLegs:     int32(w.MaxLegs),
		PlanPath:    w.PlanPath,
		SessionId:   w.SessionID,
		RepoId:      w.RepoID,
		StartedAt:   timestamppb.New(w.CreatedAt),
		UpdatedAt:   timestamppb.New(w.UpdatedAt),
	}
	if w.LastError != nil {
		p.LastError = *w.LastError
	}
	return p
}

func workflowStatusToProto(s models.WorkflowStatus) pb.WorkflowStatus {
	switch s {
	case models.WorkflowStatusPending:
		return pb.WorkflowStatus_WORKFLOW_STATUS_PENDING
	case models.WorkflowStatusRunning:
		return pb.WorkflowStatus_WORKFLOW_STATUS_RUNNING
	case models.WorkflowStatusPaused:
		return pb.WorkflowStatus_WORKFLOW_STATUS_PAUSED
	case models.WorkflowStatusCompleted:
		return pb.WorkflowStatus_WORKFLOW_STATUS_COMPLETED
	case models.WorkflowStatusFailed:
		return pb.WorkflowStatus_WORKFLOW_STATUS_FAILED
	case models.WorkflowStatusCancelled:
		return pb.WorkflowStatus_WORKFLOW_STATUS_CANCELLED
	default:
		return pb.WorkflowStatus_WORKFLOW_STATUS_UNSPECIFIED
	}
}

func workflowStepToProto(s models.WorkflowStep) pb.WorkflowStep {
	switch s {
	case models.WorkflowStepPlan:
		return pb.WorkflowStep_WORKFLOW_STEP_PLAN
	case models.WorkflowStepImplement:
		return pb.WorkflowStep_WORKFLOW_STEP_IMPLEMENT
	case models.WorkflowStepHandoff:
		return pb.WorkflowStep_WORKFLOW_STEP_HANDOFF
	case models.WorkflowStepResume:
		return pb.WorkflowStep_WORKFLOW_STEP_RESUME
	case models.WorkflowStepVerify:
		return pb.WorkflowStep_WORKFLOW_STEP_VERIFY
	case models.WorkflowStepLand:
		return pb.WorkflowStep_WORKFLOW_STEP_LAND
	default:
		return pb.WorkflowStep_WORKFLOW_STEP_UNSPECIFIED
	}
}

// workflowPriority returns a priority for a workflow status when choosing
// which active workflow to display. Higher is preferred.
func workflowPriority(s models.WorkflowStatus) int {
	switch s {
	case models.WorkflowStatusRunning:
		return 4
	case models.WorkflowStatusPending:
		return 3
	case models.WorkflowStatusPaused:
		return 2
	case models.WorkflowStatusFailed, models.WorkflowStatusCancelled:
		return 1
	default:
		return 0
	}
}

// protoToTimestamp converts an optional protobuf Timestamp to *time.Time.
func protoToTimestamp(ts *timestamppb.Timestamp) *time.Time {
	if ts == nil {
		return nil
	}
	t := ts.AsTime()
	return &t
}
