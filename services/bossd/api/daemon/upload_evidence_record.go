package server

import (
	"context"
	"slices"
	"strings"

	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossd/internal/domain"
)

// recordPublishedPage hands the proof page a successful upload published to
// the evidence reader for every session bound to repo's PR, so the session
// serves the page at once instead of after the reader's next comment read.
// The reader keeps it only for a session at msg's head. Only this daemon's own
// uploads reach it: no RPC lets a caller name a proof page.
func (s *Server) recordPublishedPage(ctx context.Context, repo *domain.Repo, msg *pb.UploadEvidenceRequest, resp *pb.UploadEvidenceResponse) {
	page := publishedPageURL(resp)
	if s.evidence == nil || repo == nil || page == "" {
		return
	}
	sessions, err := s.sessions.ListByRepoAndPR(ctx, repo.ID, int(msg.PrNumber))
	if err != nil {
		s.logger.Warn().Err(err).Str("repo_id", repo.ID).Int32("pr_number", msg.PrNumber).Msg("record published proof page: list sessions failed")
		return
	}
	for _, sess := range sessions {
		if sess != nil && sess.Session != nil {
			s.evidence.RecordPublishedPage(ctx, sess.ID, int(msg.PrNumber), msg.HeadSha, page)
		}
	}
}

// publishedPageURL is the proof page an upload published, derived from its
// response as the verify toolbox derives it: the hosted page_url, or the
// local page under the public base URL. It is empty when the upload carried
// no index.html.
func publishedPageURL(resp *pb.UploadEvidenceResponse) string {
	if resp.GetPageUrl() != "" {
		return resp.GetPageUrl()
	}
	key := resp.GetKeyPrefix() + evidencePageName
	if resp.GetKeyPrefix() == "" || !slices.Contains(resp.GetObjectKeys(), key) {
		return ""
	}
	return strings.TrimRight(resp.GetPublicBaseUrl(), "/") + "/" + key
}
