// Package server implements the ConnectRPC DaemonService handler.
package server

import (
	"fmt"
	"strings"
	"time"

	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/models"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// repoToProto converts a domain Repo to its protobuf representation.
func repoToProto(r *models.Repo) *pb.Repo {
	p := &pb.Repo{
		Id:                r.ID,
		DisplayName:       r.DisplayName,
		LocalPath:         r.LocalPath,
		OriginUrl:         r.OriginURL,
		DefaultBaseBranch: r.DefaultBaseBranch,
		WorktreeBaseDir:   r.WorktreeBaseDir,
		CreatedAt:         timestamppb.New(r.CreatedAt),
		UpdatedAt:         timestamppb.New(r.UpdatedAt),
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

// constructPRURL constructs a GitHub PR URL from an origin URL and PR number.
// Returns empty string if the origin URL cannot be parsed.
func constructPRURL(originURL string, prNumber int) string {
	// Handle SSH format: git@github.com:owner/repo.git
	s := originURL
	if idx := strings.Index(s, ":"); idx > 0 && !strings.Contains(s[:idx], "/") {
		s = s[idx+1:]
	}
	// Strip protocol prefix.
	for _, prefix := range []string{"https://", "http://", "ssh://"} {
		s = strings.TrimPrefix(s, prefix)
	}
	// Strip .git suffix and leading host.
	s = strings.TrimSuffix(s, ".git")
	// s is now e.g. "github.com/owner/repo"
	parts := strings.SplitN(s, "/", 2)
	if len(parts) != 2 {
		return ""
	}
	return fmt.Sprintf("https://%s/%s/pull/%d", parts[0], parts[1], prNumber)
}

// protoToTimestamp converts an optional protobuf Timestamp to *time.Time.
func protoToTimestamp(ts *timestamppb.Timestamp) *time.Time {
	if ts == nil {
		return nil
	}
	t := ts.AsTime()
	return &t
}
