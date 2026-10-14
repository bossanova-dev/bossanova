package github

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/recurser/bossalib/vcs"
)

// rawIssueComment is the subset of a REST issue comment the marker read needs.
type rawIssueComment struct {
	Body string `json:"body"`
	User struct {
		Login string `json:"login"`
	} `json:"user"`
	AuthorAssociation string    `json:"author_association"`
	CreatedAt         time.Time `json:"created_at"`
	UpdatedAt         time.Time `json:"updated_at"`
}

// writtenAt is when the comment's body was last written: its update time, or
// its creation time when it was never edited.
func (c rawIssueComment) writtenAt() time.Time {
	if c.UpdatedAt.After(c.CreatedAt) {
		return c.UpdatedAt
	}
	return c.CreatedAt
}

// FindPRCommentsWithMarker implements vcs.Provider. It reads every page of the
// PR's conversation (issue) comments — GitHub returns them oldest first with
// no server-side ordering option on this endpoint — and returns the marked
// ones most recently written first. An evidence comment is edited in place on
// every verify run, so the update time, not the creation time, is when its
// current body was recorded.
func (p *Provider) FindPRCommentsWithMarker(ctx context.Context, repoPath string, prID int, marker string) ([]vcs.PRComment, error) {
	if strings.TrimSpace(marker) == "" {
		return nil, errors.New("find PR comments: marker is empty")
	}
	out, err := p.gh(ctx,
		"api", fmt.Sprintf("repos/%s/issues/%d/comments?per_page=100", repoFlag(repoPath), prID),
		"--paginate",
	)
	if err != nil {
		return nil, fmt.Errorf("list PR comments: %w", err)
	}
	comments, err := decodePaginatedPages[rawIssueComment](out, "PR comments")
	if err != nil {
		return nil, err
	}
	var marked []vcs.PRComment
	for _, c := range comments {
		if !strings.Contains(c.Body, marker) {
			continue
		}
		marked = append(marked, vcs.PRComment{
			Body:              c.Body,
			AuthorLogin:       c.User.Login,
			AuthorAssociation: c.AuthorAssociation,
			WrittenAt:         c.writtenAt(),
		})
	}
	vcs.SortPRCommentsNewestFirst(marked)
	return marked, nil
}
