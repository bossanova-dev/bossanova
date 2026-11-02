package github

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/recurser/bossalib/kit/vcs"
)

const (
	// openPRSnapshotPageSize is smaller than the verify gate's 100: each PR
	// carries up to 150 nested review and check nodes, and a smaller page keeps
	// one response well inside GitHub's per-query time budget.
	openPRSnapshotPageSize = 50
	// openPRSnapshotMaxPages bounds one snapshot fetch. A repository with more
	// open PRs than this fails the fetch and every read falls back to per-PR.
	openPRSnapshotMaxPages = 10
)

// openPRSnapshotQuery reads every open PR's status, reviews, review threads
// and head check rollup for one repository, one page at a time.
var openPRSnapshotQuery = fmt.Sprintf(`query($owner: String!, $name: String!, $cursor: String) {
  repository(owner: $owner, name: $name) {
    pullRequests(states: OPEN, first: %d, after: $cursor, orderBy: {field: CREATED_AT, direction: DESC}) {
      pageInfo { hasNextPage endCursor }
      nodes {
        number title body state isDraft headRefName baseRefName headRefOid isCrossRepository
        mergeable mergeStateStatus canBeRebased reviewDecision
        author { login __typename }
        reviews(last: 50) {
          totalCount
          pageInfo { hasPreviousPage }
          nodes { id state authorAssociation updatedAt comments { totalCount } author { login __typename } }
        }
        reviewThreads(first: 100) {
          totalCount
          pageInfo { hasNextPage }
          nodes { id isResolved isOutdated }
        }
        commits(last: 1) { nodes { commit { oid statusCheckRollup { contexts(first: 100) {
          pageInfo { hasNextPage }
          nodes {
            __typename
            ... on CheckRun { name status conclusion startedAt checkSuite { workflowRun { event workflow { name } } } }
            ... on StatusContext { context state description }
          }
        } } } } }
      }
    }
  }
}`, openPRSnapshotPageSize)

// rollupContext is one status-check rollup node: a CheckRun or a StatusContext.
type rollupContext struct {
	Typename   string    `json:"__typename"`
	Name       string    `json:"name"`
	Status     string    `json:"status"`
	Conclusion string    `json:"conclusion"`
	StartedAt  time.Time `json:"startedAt"`
	CheckSuite struct {
		WorkflowRun *struct {
			Event    string `json:"event"`
			Workflow struct {
				Name string `json:"name"`
			} `json:"workflow"`
		} `json:"workflowRun"`
	} `json:"checkSuite"`
	Context     string `json:"context"`
	State       string `json:"state"`
	Description string `json:"description"`
}

func (c rollupContext) workflow() (name, event string) {
	if c.CheckSuite.WorkflowRun == nil {
		return "", ""
	}
	return c.CheckSuite.WorkflowRun.Workflow.Name, c.CheckSuite.WorkflowRun.Event
}

// snapshotPR is one open PR node of openPRSnapshotQuery.
type snapshotPR struct {
	// The embedded fields decode the shared status fields. Its reviews list
	// is filled from ReviewPage, whose shallower "reviews" key wins the decode.
	prStatusFields
	Number       int  `json:"number"`
	CanBeRebased bool `json:"canBeRebased"`
	Author       struct {
		Login    string `json:"login"`
		Typename string `json:"__typename"`
	} `json:"author"`
	ReviewPage struct {
		TotalCount int `json:"totalCount"`
		PageInfo   struct {
			HasPreviousPage bool `json:"hasPreviousPage"`
		} `json:"pageInfo"`
		Nodes []snapshotReview `json:"nodes"`
	} `json:"reviews"`
	ReviewThreads struct {
		TotalCount int `json:"totalCount"`
		PageInfo   struct {
			HasNextPage bool `json:"hasNextPage"`
		} `json:"pageInfo"`
		Nodes []snapshotThread `json:"nodes"`
	} `json:"reviewThreads"`
	Commits struct {
		Nodes []struct {
			Commit struct {
				StatusCheckRollup *struct {
					Contexts struct {
						PageInfo struct {
							HasNextPage bool `json:"hasNextPage"`
						} `json:"pageInfo"`
						Nodes []rollupContext `json:"nodes"`
					} `json:"contexts"`
				} `json:"statusCheckRollup"`
			} `json:"commit"`
		} `json:"nodes"`
	} `json:"commits"`
	// activity is the review-activity fingerprint computed at decode, or ""
	// when the snapshot's review or thread window is truncated.
	activity string
}

// snapshotReview is one review node: the fields a vcs.PRStatus reads, plus
// the edit time and inline-comment count only the fingerprint reads.
type snapshotReview struct {
	prReview
	UpdatedAt string `json:"updatedAt"`
	Comments  struct {
		TotalCount int `json:"totalCount"`
	} `json:"comments"`
}

// snapshotThread is one review thread's state, read only by the fingerprint.
type snapshotThread struct {
	ID         string `json:"id"`
	IsResolved bool   `json:"isResolved"`
	IsOutdated bool   `json:"isOutdated"`
}

type openPRSnapshotPage struct {
	Data struct {
		Repository *struct {
			PullRequests *struct {
				PageInfo struct {
					HasNextPage bool   `json:"hasNextPage"`
					EndCursor   string `json:"endCursor"`
				} `json:"pageInfo"`
				Nodes []*snapshotPR `json:"nodes"`
			} `json:"pullRequests"`
		} `json:"repository"`
	} `json:"data"`
	Errors []struct {
		Message string `json:"message"`
	} `json:"errors"`
}

// prSnapshot is every open PR of one repository, read in one paged query. Its
// projections decline (ok=false) whenever they could not reproduce the per-PR
// read exactly, so the caller falls back to that read.
type prSnapshot struct {
	p     *Provider
	order []int
	prs   map[int]*snapshotPR
	// completedAt is when the last page returned. A page can read a PR after
	// the fetch began, so only completedAt bounds the review activity the
	// snapshot's fingerprints record.
	completedAt time.Time
}

// fetchOpenPRSnapshot reads the repository's open PRs through p.gh, so the
// query sits behind the rate-limit breaker and spends the GraphQL quota. Any
// page error, a page without a cursor, or more than openPRSnapshotMaxPages
// pages fails the whole fetch: a partial snapshot is never returned.
func (p *Provider) fetchOpenPRSnapshot(ctx context.Context, repoPath string) (*prSnapshot, error) {
	nwo := repoFlag(repoPath)
	owner, name, ok := splitNWO(nwo)
	if !ok {
		return nil, fmt.Errorf("open PR snapshot: invalid GitHub repo %q", nwo)
	}
	snap := &prSnapshot{p: p, prs: make(map[int]*snapshotPR)}
	cursor := ""
	for page := 0; page < openPRSnapshotMaxPages; page++ {
		args := []string{"api", "graphql", "-f", "query=" + openPRSnapshotQuery, "-f", "owner=" + owner, "-f", "name=" + name}
		if cursor != "" {
			args = append(args, "-f", "cursor="+cursor)
		}
		out, err := p.gh(ctx, args...)
		if err != nil {
			return nil, fmt.Errorf("open PR snapshot: %w", err)
		}
		next, err := snap.addPage(out)
		if err != nil {
			return nil, fmt.Errorf("open PR snapshot: %w", err)
		}
		if next == "" {
			snap.completedAt = p.nowFn()
			return snap, nil
		}
		cursor = next
	}
	return nil, fmt.Errorf("open PR snapshot: more than %d pages of open PRs", openPRSnapshotMaxPages)
}

// addPage decodes one page into the snapshot and returns the next page's
// cursor, or "" after the last page.
func (s *prSnapshot) addPage(out string) (string, error) {
	var page openPRSnapshotPage
	if err := json.Unmarshal([]byte(out), &page); err != nil {
		return "", fmt.Errorf("parse page: %w", err)
	}
	if len(page.Errors) > 0 {
		return "", fmt.Errorf("graphql: %s", page.Errors[0].Message)
	}
	if page.Data.Repository == nil || page.Data.Repository.PullRequests == nil {
		return "", errors.New("parse page: no pull request connection")
	}
	conn := page.Data.Repository.PullRequests
	for _, pr := range conn.Nodes {
		if pr == nil || pr.Number <= 0 {
			return "", errors.New("parse page: malformed pull request node")
		}
		pr.Reviews = make([]prReview, len(pr.ReviewPage.Nodes))
		for i, review := range pr.ReviewPage.Nodes {
			pr.Reviews[i] = review.prReview
		}
		pr.activity = pr.reviewActivityDigest()
		if _, seen := s.prs[pr.Number]; !seen {
			s.order = append(s.order, pr.Number)
		}
		s.prs[pr.Number] = pr
	}
	if !conn.PageInfo.HasNextPage {
		return "", nil
	}
	if conn.PageInfo.EndCursor == "" {
		return "", errors.New("parse page: another page reported with no endCursor")
	}
	return conn.PageInfo.EndCursor, nil
}

// status projects one PR's vcs.PRStatus through buildPRStatus. It declines a
// PR absent from the snapshot, an open ready PR whose mergeability GitHub has
// not computed (the per-PR read's REST call is what prompts GitHub to compute
// it), and a PR with older reviews beyond the fetched page when none of the
// fetched ones moved LatestReviewState.
func (s *prSnapshot) status(prID int) (*vcs.PRStatus, bool) {
	pr, ok := s.prs[prID]
	if !ok {
		return nil, false
	}
	var mergeability *pullRequestMergeability
	if pr.needsMergeability() {
		if pr.Mergeable != "MERGEABLE" {
			return nil, false
		}
		// The REST read reports the same GraphQL mergeable and merge state;
		// only rebaseability is new.
		rebaseable := pr.CanBeRebased
		mergeability = &pullRequestMergeability{Rebaseable: &rebaseable}
	}
	moved := false
	trusted := func(review prReview) bool {
		login := review.Author.Login
		if review.Author.Typename == "Bot" {
			login += "[bot]"
		}
		if !vcs.IsTrustedReviewAuthor(login, review.AuthorAssociation) {
			s.p.logUntrustedReview(prID, review.Author.Login, review.AuthorAssociation, "pr snapshot")
			return false
		}
		if parseReviewState(review.State) != vcs.ReviewStateUnspecified {
			moved = true
		}
		return true
	}
	status := buildPRStatus(pr.prStatusFields, mergeability, trusted)
	if pr.ReviewPage.PageInfo.HasPreviousPage && !moved {
		return nil, false
	}
	return status, true
}

// checkSet projects one PR's head checks through buildCheckSet, in the rows
// `gh pr checks` would print. It declines a PR absent from the snapshot and a
// rollup with more contexts than one page holds.
func (s *prSnapshot) checkSet(prID int) (vcs.CheckSet, bool) {
	pr, ok := s.prs[prID]
	if !ok {
		return vcs.CheckSet{}, false
	}
	var contexts []rollupContext
	for _, node := range pr.Commits.Nodes {
		rollup := node.Commit.StatusCheckRollup
		if rollup == nil {
			continue
		}
		if rollup.Contexts.PageInfo.HasNextPage {
			return vcs.CheckSet{}, false
		}
		contexts = append(contexts, rollup.Contexts.Nodes...)
	}
	return s.p.buildCheckSet(checkRows(contexts)), true
}

// checkRows converts rollup contexts into `gh pr checks` rows the way gh does
// (cli/cli pkg/cmd/pr/checks aggregate.go): newest first by start time, one
// row per status context and per (check name, workflow, event), a check run's
// state is its conclusion once completed and its status before, and gh reads
// no description for a check run.
func checkRows(contexts []rollupContext) []checkRow {
	sorted := append([]rollupContext(nil), contexts...)
	sort.SliceStable(sorted, func(i, j int) bool { return sorted[i].StartedAt.After(sorted[j].StartedAt) })
	seen := make(map[string]bool, len(sorted))
	rows := make([]checkRow, 0, len(sorted))
	for _, c := range sorted {
		if c.Typename == "StatusContext" {
			if seen["status|"+c.Context] {
				continue
			}
			seen["status|"+c.Context] = true
			rows = append(rows, checkRow{Name: c.Context, State: c.State, Description: c.Description})
			continue
		}
		workflow, event := c.workflow()
		key := "check|" + c.Name + "/" + workflow + "/" + event
		if seen[key] {
			continue
		}
		seen[key] = true
		state := c.Status
		if c.Status == "COMPLETED" {
			state = c.Conclusion
		}
		rows = append(rows, checkRow{Name: c.Name, State: state, Workflow: workflow})
	}
	return rows
}

// openPRs projects the open-PR list in `gh pr list` form: newest first, with a
// bot author's login spelled app/<login> as gh spells it.
func (s *prSnapshot) openPRs() []vcs.PRSummary {
	prs := make([]vcs.PRSummary, 0, len(s.order))
	for _, n := range s.order {
		pr := s.prs[n]
		author := pr.Author.Login
		if pr.Author.Typename != "" && pr.Author.Typename != "User" {
			author = "app/" + author
		}
		prs = append(prs, vcs.PRSummary{
			Number:     pr.Number,
			Title:      pr.Title,
			HeadBranch: pr.HeadRefName,
			State:      parsePRState(pr.State),
			Author:     author,
		})
	}
	return prs
}

// reviewActivity returns one PR's review-activity fingerprint: a digest of
// every input fetchReviewComments reads that the snapshot can observe (head
// commit, the review set with each review's state, author, association, edit
// time and inline-comment count, and every thread's resolution and outdated
// state). Equal fingerprints mean the snapshot saw no review activity between
// them. It declines (ok=false) a PR absent from the snapshot and one whose
// review or thread window is truncated, since activity outside the window is
// unseen. An inline comment's body edit moves none of these inputs; webhooks
// and reviewCommentsMaxAge cover it.
func (s *prSnapshot) reviewActivity(prID int) (string, bool) {
	pr, ok := s.prs[prID]
	if !ok || pr.activity == "" {
		return "", false
	}
	return pr.activity, true
}

// reviewActivityDigest computes pr's fingerprint, or "" when its review or
// thread window is truncated. The serialization is deterministic: fixed field
// order, nodes in GitHub's returned order, and counts included so a removal
// moves the digest.
func (pr *snapshotPR) reviewActivityDigest() string {
	if pr.ReviewPage.PageInfo.HasPreviousPage || pr.ReviewThreads.PageInfo.HasNextPage {
		return ""
	}
	type review struct {
		ID, State, Association, Login, Typename, UpdatedAt string
		Comments                                           int
	}
	input := struct {
		Head        string
		ReviewCount int
		Reviews     []review
		ThreadCount int
		Threads     []snapshotThread
	}{
		Head:        pr.HeadRefOid,
		ReviewCount: pr.ReviewPage.TotalCount,
		Reviews:     make([]review, 0, len(pr.ReviewPage.Nodes)),
		ThreadCount: pr.ReviewThreads.TotalCount,
		Threads:     pr.ReviewThreads.Nodes,
	}
	for _, r := range pr.ReviewPage.Nodes {
		input.Reviews = append(input.Reviews, review{
			ID: r.ID, State: r.State, Association: r.AuthorAssociation, Login: r.Author.Login,
			Typename: r.Author.Typename, UpdatedAt: r.UpdatedAt, Comments: r.Comments.TotalCount,
		})
	}
	// Marshalling plain strings, ints and bools cannot fail.
	b, _ := json.Marshal(input)
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}
