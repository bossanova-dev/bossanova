// Package report assembles the context payload that the TUI submits with a
// bug report. It snapshots session state, identity, and log tails so that a
// human triaging the report has enough to reproduce without a back-and-forth.
package report

import (
	"context"
	"os"
	"runtime"

	"github.com/rs/zerolog/log"

	"github.com/recurser/boss/internal/client"
	"github.com/recurser/bossalib/buildinfo"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	bossalog "github.com/recurser/bossalib/log"
)

// logTailLines is how many lines of each log file get attached to a report.
// 200 keeps payloads small (< ~50 KB combined even with long lines) while
// still showing the full crash/shutdown sequence that usually fits in the
// last few dozen lines.
const logTailLines = 200

// CollectContext assembles the bug-report context. It never returns an error
// for partial data — a log file that doesn't exist yet, an empty session
// list, a nil current session, or a failed ListSessions RPC all just leave
// the corresponding fields empty. Users are most likely to file a report
// when the daemon is misbehaving (exactly when ListSessions is likely to
// fail), so surfacing those failures as a report-submission error would
// block the comment from reaching triage over purely supplementary data.
func CollectContext(
	ctx context.Context,
	c client.BossClient,
	current *pb.Session,
	daemonStatuses map[string]string,
) (*pb.ReportContext, error) {
	rc := &pb.ReportContext{
		BossVersion:    buildinfo.Version,
		BossCommit:     buildinfo.Commit,
		Os:             runtime.GOOS,
		Arch:           runtime.GOARCH,
		Terminal:       os.Getenv("TERM"),
		CurrentSession: current,
		DaemonStatuses: daemonStatuses,
	}

	sessions, err := c.ListSessions(ctx, &pb.ListSessionsRequest{IncludeArchived: false})
	if err != nil {
		log.Warn().Err(err).Msg("report: ListSessions failed; submitting report without session list")
	}
	rc.Sessions = make([]*pb.SessionSummary, 0, len(sessions))
	for _, s := range sessions {
		rc.Sessions = append(rc.Sessions, &pb.SessionSummary{
			Id:        s.Id,
			RepoId:    s.RepoId,
			Title:     s.Title,
			State:     s.State,
			PrNumber:  s.PrNumber,
			PrUrl:     s.PrUrl,
			UpdatedAt: s.UpdatedAt,
		})
	}

	rc.BossLogTail = tailLog("boss")
	rc.BossdLogTail = tailLog("bossd")

	return rc, nil
}

// tailLog returns the last logTailLines of the given service's log file, or
// "" if the file doesn't exist or can't be read. Swallowing the error is
// intentional: missing logs shouldn't block a bug report — the submitter
// still gets their comment through, and the triager sees an empty tail.
func tailLog(service string) string {
	path := bossalog.LogPath(service)
	if path == "" {
		return ""
	}
	tail, err := bossalog.Tail(path, logTailLines)
	if err != nil {
		return ""
	}
	return tail
}
