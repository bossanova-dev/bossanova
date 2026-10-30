package session

import (
	"context"
	"fmt"
	"strings"

	"github.com/recurser/bossalib/kit/vcs"
	"github.com/recurser/bossalib/models"
	gitpkg "github.com/recurser/bossd/internal/adapter/git"
)

// finalizeTitle decides the PR/session title for a session at cron finalize.
//
// The historical behavior derived the title from the LAST commit subject. That
// is fine when the branch is a single commit (the subject IS the PR's intent),
// but wrong once the branch carries several commits — the last one is usually a
// narrow fix, so the derived title is irrelevant and, worse, it CLOBBERS a good
// title the agent set at PR creation (the WON-1280 bug: "[WON-1280] Don't allow …"
// became "Fail closed when clean-tree baseline is missing").
//
// So the policy is:
//   - ≥2 real commits: ask the agent (which keeps a good existing title and only
//     improves a weak one). If it has no suggestion, PRESERVE the existing PR
//     title rather than rewriting it to the last commit subject.
//   - 0–1 real commits: derive deterministically by normalizing the commit
//     subject (the old, well-tested behavior — cleans a messy "type(scope): …"
//     PR title into a readable one).
//   - commit history unreadable: preserve the existing title (never risk a
//     clobber on incomplete information).
//
// currentPRTitle is the PR's current title (set earlier, e.g. by the agent at PR
// creation). Non-cron sessions keep their own title unchanged.
func (l *Lifecycle) finalizeTitle(ctx context.Context, session *models.Session, currentPRTitle string) string {
	rawCurrent := currentPRTitle
	currentPRTitle = unmarkNoCommitsPRTitle(currentPRTitle)
	if session.CronJobID == nil || *session.CronJobID == "" {
		return session.Title
	}

	if l.isFinalizePlaceholderTitle(ctx, session, rawCurrent) {
		currentPRTitle = ""
	}
	subjects, err := l.worktrees.CommitSubjects(ctx, session.WorktreePath, session.BaseBranch)
	if err != nil {
		l.logger.Warn().Err(err).
			Str("session", session.ID).
			Str("base", session.BaseBranch).
			Msg("failed to read commit subjects for cron PR title; preserving existing title")
		if t := strings.TrimSpace(currentPRTitle); t != "" {
			return t
		}
		return l.draftPRTitle(ctx, session)
	}

	if real := realCommitSubjects(subjects); len(real) >= 2 {
		if title, ok := l.agentSuggestedTitle(ctx, session, currentPRTitle, real); ok {
			return title
		}
		// Multi-commit with no agent suggestion: never rewrite to the last commit
		// subject. Preserve a meaningful existing title; derive only if none.
		if t := strings.TrimSpace(currentPRTitle); t != "" {
			return t
		}
	}
	return l.draftPRTitle(ctx, session)
}

// realCommitSubjects drops the empty draft-PR placeholder commit so it never
// counts toward the multi-commit threshold or reaches the title suggester.
//
// Classification is delegated to gitpkg.IsDraftPRPlaceholderSubject: package
// git owns both the placeholder literal and the tolerance for the "[#NNN] "
// tag inject-pr-tag can leave in it (BOS-591). Keeping one implementation
// means the finalize guard and the PR-tag injector can never disagree about
// what counts as real work.
func realCommitSubjects(raw []string) []string {
	out := make([]string, 0, len(raw))
	for _, s := range raw {
		if t := strings.TrimSpace(s); t == "" || gitpkg.IsDraftPRPlaceholderSubject(t) {
			continue
		}
		out = append(out, s)
	}
	return out
}

func (l *Lifecycle) isFinalizePlaceholderTitle(ctx context.Context, session *models.Session, rawCurrent string) bool {
	title := unmarkNoCommitsPRTitle(rawCurrent)
	if hasNoCommitsPRTitleMarker(rawCurrent) && (title == session.Title || title == normalizeCronPRTitle(session.Title)) {
		return true
	}
	if l.cronJobs == nil {
		return false
	}
	job, err := l.cronJobs.Get(ctx, *session.CronJobID)
	return err == nil && job != nil && isCronDefaultTitle(title, job.Name)
}

func withTrackerTitlePrefix(title string, trackerID *string) string {
	if trackerID == nil || *trackerID == "" || title == "" {
		return title
	}
	prefix := "[" + *trackerID + "]"
	if strings.Contains(strings.ToLower(title), strings.ToLower(prefix)) {
		return title
	}
	if hasNoCommitsPRTitleMarker(title) {
		return markNoCommitsPRTitle(prefix + " " + unmarkNoCommitsPRTitle(title))
	}
	return prefix + " " + title
}

// WriteSessionPRTitle preserves the tracker linkage on every daemon title write.
// It returns the written title so callers can persist the same value.
func WriteSessionPRTitle(ctx context.Context, provider vcs.Provider, originURL string, sess *models.Session, prNumber int, title string) (string, error) {
	title = withTrackerTitlePrefix(title, sess.TrackerID)
	return title, provider.UpdatePRTitle(ctx, originURL, prNumber, title)
}

func (l *Lifecycle) attachedPRTitle(ctx context.Context, session *models.Session, originURL string, pr vcs.PRSummary) (string, error) {
	if session.PRNumber != nil && !hasNoCommitsPRTitleMarker(pr.Title) {
		return "", nil
	}
	if !isCronSession(session) {
		return pr.Title, nil
	}
	title := withTrackerTitlePrefix(l.finalizeTitle(ctx, session, pr.Title), session.TrackerID)
	if title == pr.Title {
		return title, nil
	}
	written, err := WriteSessionPRTitle(ctx, l.provider, originURL, session, pr.Number, title)
	if err != nil {
		return "", fmt.Errorf("update attached PR title: %w", err)
	}
	return written, nil
}
