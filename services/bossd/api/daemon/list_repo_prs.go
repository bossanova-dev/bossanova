package server

import (
	"cmp"
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	"connectrpc.com/connect"

	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/kit/vcs"
	"github.com/recurser/bossalib/models"
)

// ownedPR is one open PR with the non-archived session that owns it, if any.
type ownedPR struct {
	pr    vcs.PRSummary
	owner *models.Session
}

// ListRepoPRs returns a repo's open PRs. Without a query it hides PRs that a
// duplicate-blocking session owns, keeps the provider's order and applies no
// cap, which is what the New session pickers expect. A query can turn that
// exclusion off or hide every owned PR, filter by author and target branch,
// order by created or updated time, and cap the result.
func (s *Server) ListRepoPRs(ctx context.Context, req *connect.Request[pb.ListRepoPRsRequest]) (*connect.Response[pb.ListRepoPRsResponse], error) {
	if req.Msg.RepoId == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("repo_id is required"))
	}
	query := req.Msg.GetQuery()
	if err := validatePRListQuery(query); err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, err)
	}

	// Verify the repo exists and get its origin URL.
	repo, err := s.repos.Get(ctx, req.Msg.RepoId)
	if err != nil {
		return nil, connect.NewError(connect.CodeNotFound, fmt.Errorf("repo not found: %w", err))
	}
	if repo.OriginURL == "" {
		return connect.NewResponse(&pb.ListRepoPRsResponse{}), nil
	}

	// Foreground read: the pickers need live data. This is one list query.
	prs, err := s.provider.ListOpenPRs(ctx, repo.OriginURL)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("list PRs: %w", err))
	}
	sessions, err := s.sessions.ListActive(ctx, req.Msg.RepoId)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("list active sessions: %w", err))
	}

	rows := filterPRsByOwner(prs, sessions, query.GetOwnerFilter())
	rows = filterPRs(rows, query.GetAuthor(), query.GetBaseBranch())
	sortPRs(rows, query.GetOrder())
	if limit := int(query.GetLimit()); limit > 0 && len(rows) > limit {
		rows = rows[:limit]
	}

	pbPRs := make([]*pb.PRSummary, len(rows))
	for i, row := range rows {
		pbPRs[i] = s.prSummaryToProto(row)
	}
	return connect.NewResponse(&pb.ListRepoPRsResponse{PullRequests: pbPRs}), nil
}

// validatePRListQuery rejects an order or owner filter that is not a defined
// enum value. A nil query is valid.
func validatePRListQuery(query *pb.PRListQuery) error {
	if _, ok := pb.PRListOrder_name[int32(query.GetOrder())]; !ok {
		return fmt.Errorf("unknown order %d; valid values: %s", query.GetOrder(), enumNames(pb.PRListOrder_name))
	}
	if _, ok := pb.PRListOwnerFilter_name[int32(query.GetOwnerFilter())]; !ok {
		return fmt.Errorf("unknown owner_filter %d; valid values: %s", query.GetOwnerFilter(), enumNames(pb.PRListOwnerFilter_name))
	}
	return nil
}

func enumNames(names map[int32]string) string {
	keys := make([]int32, 0, len(names))
	for k := range names {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	out := make([]string, len(keys))
	for i, k := range keys {
		out[i] = names[k]
	}
	return strings.Join(out, ", ")
}

// filterPRsByOwner pairs each PR with its owning session and applies the
// owner filter. UNSPECIFIED hides PRs a duplicate-blocking session owns (the
// pickers' rule), ALL keeps every PR, and UNOWNED keeps only PRs no
// non-archived session owns.
func filterPRsByOwner(prs []vcs.PRSummary, sessions []*models.Session, filter pb.PRListOwnerFilter) []ownedPR {
	if filter == pb.PRListOwnerFilter_PR_LIST_OWNER_FILTER_UNSPECIFIED {
		prs = newActiveSessionKeys(sessions).excludePRs(prs)
	}
	owners := newPROwnerIndex(sessions)
	rows := make([]ownedPR, 0, len(prs))
	for _, pr := range prs {
		owner := owners.owner(pr)
		if owner != nil && filter == pb.PRListOwnerFilter_PR_LIST_OWNER_FILTER_UNOWNED {
			continue
		}
		rows = append(rows, ownedPR{pr: pr, owner: owner})
	}
	return rows
}

// filterPRs keeps rows matching author and baseBranch; an empty value matches
// every row. The author match is case-insensitive, and a bot reported as
// app/<login> also matches <login>.
func filterPRs(rows []ownedPR, author, baseBranch string) []ownedPR {
	if author == "" && baseBranch == "" {
		return rows
	}
	out := make([]ownedPR, 0, len(rows))
	for _, row := range rows {
		if author != "" && !prAuthorMatches(row.pr.Author, author) {
			continue
		}
		if baseBranch != "" && row.pr.BaseBranch != baseBranch {
			continue
		}
		out = append(out, row)
	}
	return out
}

func prAuthorMatches(prAuthor, want string) bool {
	return strings.EqualFold(prAuthor, want) || strings.EqualFold(strings.TrimPrefix(prAuthor, "app/"), want)
}

// sortPRs stably orders rows by created or updated time, breaking ties with
// the higher PR number first. UNSPECIFIED keeps the provider's order.
func sortPRs(rows []ownedPR, order pb.PRListOrder) {
	var at func(vcs.PRSummary) time.Time
	descending := false
	switch order {
	case pb.PRListOrder_PR_LIST_ORDER_CREATED_DESC:
		at, descending = func(pr vcs.PRSummary) time.Time { return pr.CreatedAt }, true
	case pb.PRListOrder_PR_LIST_ORDER_CREATED_ASC:
		at = func(pr vcs.PRSummary) time.Time { return pr.CreatedAt }
	case pb.PRListOrder_PR_LIST_ORDER_UPDATED_DESC:
		at, descending = func(pr vcs.PRSummary) time.Time { return pr.UpdatedAt }, true
	case pb.PRListOrder_PR_LIST_ORDER_UPDATED_ASC:
		at = func(pr vcs.PRSummary) time.Time { return pr.UpdatedAt }
	default:
		return
	}
	slices.SortStableFunc(rows, func(a, b ownedPR) int {
		c := at(a.pr).Compare(at(b.pr))
		if descending {
			c = -c
		}
		if c != 0 {
			return c
		}
		return cmp.Compare(b.pr.Number, a.pr.Number)
	})
}

// prSummaryToProto converts one row, naming its owning session and that
// session's display status when the display tracker has one.
func (s *Server) prSummaryToProto(row ownedPR) *pb.PRSummary {
	pr := row.pr
	out := &pb.PRSummary{
		Number:     clampInt32(pr.Number),
		Title:      pr.Title,
		HeadBranch: pr.HeadBranch,
		State:      pb.PRState(clampInt32(int(pr.State))),
		Author:     pr.Author,
		HeadSha:    pr.HeadSHA,
		IsDraft:    pr.IsDraft,
		BaseBranch: pr.BaseBranch,
		CreatedAt:  timestampOrNil(pr.CreatedAt),
		UpdatedAt:  timestampOrNil(pr.UpdatedAt),
	}
	if row.owner == nil {
		return out
	}
	out.SessionId = row.owner.ID
	if s.displayTracker != nil {
		if entry := s.displayTracker.Get(row.owner.ID); entry != nil {
			out.SessionDisplayStatus = pb.DisplayStatus(clampInt32(int(entry.Status)))
		}
	}
	return out
}
