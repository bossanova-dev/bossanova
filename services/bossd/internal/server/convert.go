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
		CreatedAt:               timestamppb.New(r.CreatedAt),
		UpdatedAt:               timestamppb.New(r.UpdatedAt),
	}
	if r.SetupScript != nil {
		p.SetupScript = r.SetupScript
	}
	return p
}

// SessionToProto converts a domain Session to its protobuf representation.
func SessionToProto(s *models.Session) *pb.Session {
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
		DisplayLabel:      s.DisplayLabel,
		DisplayIntent:     pb.DisplayIntent(s.DisplayIntent),
		DisplaySpinner:    s.DisplaySpinner,
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
	if s.TrackerID != nil {
		p.TrackerId = s.TrackerID
	}
	if s.TrackerURL != nil {
		p.TrackerUrl = s.TrackerURL
	}
	if s.TmuxSessionName != nil {
		p.TmuxSessionName = s.TmuxSessionName
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
	out := &pb.ClaudeChat{
		Id:        c.ID,
		SessionId: c.SessionID,
		ClaudeId:  c.ClaudeID,
		Title:     c.Title,
		DaemonId:  c.DaemonID,
		CreatedAt: timestamppb.New(c.CreatedAt),
	}
	if c.TmuxSessionName != nil {
		out.TmuxSessionName = *c.TmuxSessionName
	}
	return out
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

// protoToTimestamp converts an optional protobuf Timestamp to *time.Time.
func protoToTimestamp(ts *timestamppb.Timestamp) *time.Time {
	if ts == nil {
		return nil
	}
	t := ts.AsTime()
	return &t
}
