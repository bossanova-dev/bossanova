package session

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"

	"github.com/recurser/bossalib/kit/vcs"
	"github.com/recurser/bossalib/models"
	"github.com/recurser/bossd/internal/db"
)

const maxTicketTagChecksPerPass = 5

var trackerLinePattern = regexp.MustCompile(`(?i)^(?:Tracker|Linear) issue:\s*\S+\s*$`)

func trackerIssueLine(url string) string { return "Tracker issue: " + url }

func hasTrackerLine(body string) bool {
	for line := range strings.SplitSeq(body, "\n") {
		if line = strings.TrimSpace(line); line != "" {
			return trackerLinePattern.MatchString(line)
		}
	}
	return false
}

func withTrackerLine(url *string, body string) string {
	if url == nil || strings.TrimSpace(*url) == "" || hasTrackerLine(body) {
		return body
	}
	return trackerIssueLine(*url) + "\n\n" + body
}

func bootstrapPRBody(sess *models.Session) string {
	return withTrackerLine(sess.TrackerURL, draftPRBody(sess.Plan, sess.TrackerID))
}

// EnsureTicketTags adds missing ticket tags independently of linking, so an
// earlier failed PR edit is retried even when the session is already linked.
func (l *TrackerLinker) EnsureTicketTags(parent context.Context) (int64, error) {
	if l == nil || l.provider == nil {
		return 0, nil
	}
	ctx, cancel := context.WithTimeout(parent, l.passTimeout)
	defer cancel()
	n, err := l.ensureTicketTags(ctx)
	if err != nil && parent.Err() == nil && errors.Is(ctx.Err(), context.DeadlineExceeded) {
		err = fmt.Errorf("%w after %s: %w", ErrTrackerLinkPassTimedOut, l.passTimeout, err)
	}
	return n, err
}

func (l *TrackerLinker) ensureTicketTags(ctx context.Context) (int64, error) {
	active, err := l.sessions.ListActive(ctx, "")
	if err != nil {
		return 0, fmt.Errorf("list active sessions: %w", err)
	}
	// Resume after the last attempt, including failures and incomplete reads.
	// Stable ID order also keeps retries fair when display ordering changes.
	active = slices.DeleteFunc(slices.Clone(active), func(s *models.Session) bool { return s == nil })
	slices.SortFunc(active, func(a, b *models.Session) int { return cmp.Compare(a.ID, b.ID) })
	l.mu.Lock()
	cursor := l.ticketTagCursor
	l.mu.Unlock()
	start := 0
	for i, sess := range active {
		if sess.ID > cursor {
			start = i
			break
		}
	}
	active = slices.Concat(active[start:], active[:start])
	var tagged int64
	checked := 0
	for _, sess := range active {
		if err := ctx.Err(); err != nil {
			return tagged, err
		}
		if checked >= maxTicketTagChecksPerPass {
			break
		}
		if sess == nil || sess.ArchivedAt != nil || sess.IsQuickChat || sess.PRNumber == nil ||
			sess.TrackerID == nil || strings.TrimSpace(*sess.TrackerID) == "" ||
			sess.TrackerURL == nil || strings.TrimSpace(*sess.TrackerURL) == "" {
			continue
		}
		key := fmt.Sprintf("%s\x00%d\x00%s\x00%s", sess.ID, *sess.PRNumber, *sess.TrackerID, *sess.TrackerURL)
		l.mu.Lock()
		done := l.ticketTags[key]
		l.mu.Unlock()
		if done {
			continue
		}
		checked++
		l.mu.Lock()
		l.ticketTagCursor = sess.ID
		l.mu.Unlock()
		complete, err := l.ensureSessionTicketTags(ctx, sess)
		if err != nil {
			l.logger.Warn().Err(err).Str("session", sess.ID).Int("pr", *sess.PRNumber).Msg("tracker tags: failed; retrying next pass")
			continue
		}
		if complete {
			l.mu.Lock()
			l.ticketTags[key] = true
			l.mu.Unlock()
			tagged++
		}
	}
	return tagged, ctx.Err()
}

func (l *TrackerLinker) ensureSessionTicketTags(ctx context.Context, sess *models.Session) (bool, error) {
	repo, err := l.repos.Get(ctx, sess.RepoID)
	if err != nil {
		return false, fmt.Errorf("get repo: %w", err)
	}
	if repo == nil || strings.TrimSpace(repo.OriginURL) == "" {
		return false, nil
	}
	// These must be fresh reads. GitHub has no conditional title/body edit; a
	// concurrent publisher can race this read-modify-write while a tag is missing.
	status, err := l.provider.GetPRStatus(ctx, repo.OriginURL, *sess.PRNumber)
	if err != nil {
		return false, fmt.Errorf("read PR: %w", err)
	}
	if status == nil {
		return false, nil
	}
	if status.State != vcs.PRStateOpen {
		return true, nil
	}
	written := status.Title
	if withTrackerTitlePrefix(status.Title, sess.TrackerID) != status.Title {
		written, err = WriteSessionPRTitle(ctx, l.provider, repo.OriginURL, sess, *sess.PRNumber, status.Title)
		if err != nil {
			return false, fmt.Errorf("write title: %w", err)
		}
	}
	if err := l.syncTicketTagTitle(ctx, sess, status.Title, written); err != nil {
		return false, err
	}
	if !hasTrackerLine(status.Body) {
		if updater, ok := l.provider.(vcs.PRBodyUpdater); ok {
			if err := updater.UpdatePRBody(ctx, repo.OriginURL, *sess.PRNumber, withTrackerLine(sess.TrackerURL, status.Body)); err != nil {
				return false, fmt.Errorf("write body: %w", err)
			}
		} else {
			l.logger.Debug().Str("session", sess.ID).Int("pr", *sess.PRNumber).Msg("tracker tags: provider has no body updater")
		}
	}
	return true, nil
}

func (l *TrackerLinker) syncTicketTagTitle(ctx context.Context, sess *models.Session, oldTitle, written string) error {
	title := unmarkNoCommitsPRTitle(written)
	if sess.Title == title {
		return nil
	}
	// Recover a local write that failed after the remote tag was added.
	syncTitle := sess.Title == unmarkNoCommitsPRTitle(oldTitle) || withTrackerTitlePrefix(sess.Title, sess.TrackerID) == title
	if !syncTitle && l.cronJobs != nil && sess.CronJobID != nil {
		job, err := l.cronJobs.Get(ctx, *sess.CronJobID)
		if err != nil {
			return fmt.Errorf("get cron job: %w", err)
		}
		syncTitle = job != nil && isCronDefaultTitle(sess.Title, job.Name)
	}
	if !syncTitle {
		return nil
	}
	updated, err := l.sessions.Update(ctx, sess.ID, db.UpdateSessionParams{Title: &title})
	if err != nil {
		return fmt.Errorf("update session title: %w", err)
	}
	if l.notify != nil {
		l.notify(ctx, updated)
	}
	return nil
}
