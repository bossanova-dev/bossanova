package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"regexp"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/recurser/boss/internal/logtail"
	"github.com/recurser/bossalib/config"
	bossalog "github.com/recurser/bossalib/log"
)

const liveMergeWindow = 10 * time.Millisecond

func tailCmd() *cobra.Command {
	var all, follow, asJSON bool
	var lines int
	var filter logtail.Filter
	cmd := &cobra.Command{
		// A cobra usage grammar, not a sentence: ellipsis: literal-dots ok
		Use: "tail [source...]", Short: "Tail daemon and agent logs",
		// Two surfaces, one command. Reaching for `boss tail` to debug an agent
		// run and getting bossd records back reads as "the agent went silent"
		// when the data simply lives in a different file, so the help states
		// which sources hold which and names the agent-log path outright.
		Long: "Show recent log lines without needing to know where the log files live.\n\n" +
			"Two separate surfaces:\n" +
			"  SERVICE logs — " + strings.Join(bossalog.Services(), ", ") + " (default bossd) — carry service\n" +
			"  records ONLY. No agent or chat output ever appears in them, however silent\n" +
			"  they look.\n\n" +
			"  AGENT/CHAT output is the other surface, read by passing an agent-session id\n" +
			"  in place of a service name. It lives at\n" +
			"  <worktree_base_dir>/../agent-logs/<agent-session-id>.log and holds either the\n" +
			"  raw tmux capture an interactive chat writes or the {\"ts\",\"text\"} JSON lines a\n" +
			"  headless run writes (stdout AND stderr, [runner] diagnostics interleaved),\n" +
			"  detected per file.\n\n" +
			"Pass several sources to interleave them by timestamp; --all merges the service logs\n" +
			"only, never the agent logs. Agent output has no level, so it always passes --level,\n" +
			"--repo and --plugin. -n counts physical lines read from each source before\n" +
			"filtering. Non-JSON lines always pass filters.",
		RunE: func(cmd *cobra.Command, args []string) error {
			// Go otherwise exits with SIGPIPE before a stdout write can return
			// EPIPE, which makes `boss tail -f | head` report status 141 instead
			// of the conventional clean tail exit. quietOnClosedPipe handles the
			// resulting write error below.
			signal.Ignore(syscall.SIGPIPE)
			settings, _ := config.Load()
			sources, notice, err := resolveSources(args, all, bossalog.AgentLogsDir(settings.WorktreeBaseDir))
			if err != nil {
				return err
			}
			if notice != "" {
				if _, err := fmt.Fprintln(cmd.OutOrStdout(), notice); err != nil {
					return quietOnClosedPipe(err)
				}
			}
			if len(sources) == 0 {
				return nil
			}
			write := func(rec logtail.Record) error {
				if !filter.Match(rec) {
					return nil
				}
				if asJSON {
					return logtail.FormatJSON(cmd.OutOrStdout(), rec)
				}
				return logtail.FormatPretty(cmd.OutOrStdout(), rec)
			}
			if !follow {
				return quietOnClosedPipe(writeBacklog(sources, lines, nil, write))
			}
			return quietOnClosedPipe(followSources(cmd.Context(), sources, lines, write, nil))
		},
	}
	cmd.Flags().BoolVar(&all, "all", false, "merge every service log")
	cmd.Flags().IntVarP(&lines, "lines", "n", 10, "physical lines to read per source before filtering")
	cmd.Flags().BoolVarP(&follow, "follow", "f", false, "keep reading as the log grows")
	cmd.Flags().StringVar(&filter.Repo, "repo", "", "only records for this repo")
	cmd.Flags().StringVar(&filter.Plugin, "plugin", "", "only records from this plugin")
	cmd.Flags().StringVar(&filter.Level, "level", "", "only records at this level")
	cmd.Flags().BoolVar(&asJSON, "json", false, "emit one parseable JSON object per line")
	return cmd
}

// tailSource is one resolved log to read. A service log is rotated and keyed
// by service name; an agent log is a single unrotated file keyed by
// agent-session id, in one of two formats the logtail package detects.
type tailSource struct {
	Name  string
	Path  string
	Agent bool
	// Seed anchors untimestamped agent output. Captured once, here, so a raw
	// pipe-pane capture never sorts to the epoch.
	Seed time.Time
}

// agentSessionID matches the identifiers bossd names agent logs after: the
// UUID minted for an agent session, and the boss session id of a repair run.
var agentSessionID = regexp.MustCompile(`^(?i:[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}|repair-[0-9a-f]+)$`)

func serviceSource(service string) tailSource {
	return tailSource{Name: service, Path: bossalog.LogPath(service)}
}

func serviceSources(services []string) []tailSource {
	sources := make([]tailSource, 0, len(services))
	for _, service := range services {
		sources = append(sources, serviceSource(service))
	}
	return sources
}

// resolveSources is the single place a source name becomes a log to read, so
// the positional arguments and --all stay coherent. It returns a notice to
// print when an agent log was asked for but the agent-logs directory does not
// exist: that is a clean message rather than a failure, because a fresh
// install has simply never run an agent.
func resolveSources(args []string, all bool, agentLogsDir string) ([]tailSource, string, error) {
	if all {
		if len(args) > 0 {
			return nil, "", errors.New("pass a source or --all, not both")
		}
		// --all stays service-only: the agent-logs directory can hold one file
		// per agent session ever run, which is not a merge anyone wants.
		return serviceSources(bossalog.Services()), "", nil
	}
	if len(args) == 0 {
		return serviceSources([]string{"bossd"}), "", nil
	}
	var sources []tailSource
	var notice string
	for _, arg := range args {
		if bossalog.KnownService(arg) {
			sources = append(sources, serviceSource(arg))
			continue
		}
		// Empty when no agent-logs directory is configured, which the
		// dirExists check below then reports as a clean notice.
		path := bossalog.AgentLogFile(agentLogsDir, arg)
		// Reject a plain typo before reporting on the agent-logs directory, so
		// an unknown source still names the sources that do exist.
		if !fileExists(path) && !agentSessionID.MatchString(arg) {
			return nil, "", fmt.Errorf("unknown log source %q (want one of: %s, or an agent-session id)",
				arg, strings.Join(bossalog.Services(), ", "))
		}
		if !dirExists(agentLogsDir) {
			notice = fmt.Sprintf("no agent logs: %s does not exist", agentLogsDirLabel(agentLogsDir))
			continue
		}
		// A log that does not exist yet is still a valid source under -f: the
		// follower waits for the agent to create it.
		sources = append(sources, tailSource{Name: arg, Path: path, Agent: true, Seed: logtail.AgentSeedTime(path)})
	}
	return sources, notice, nil
}

func agentLogsDirLabel(dir string) string {
	if dir == "" {
		return "the agent-logs directory (no worktree base directory configured)"
	}
	return dir
}

func fileExists(path string) bool {
	if path == "" {
		return false
	}
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}

func dirExists(path string) bool {
	if path == "" {
		return false
	}
	info, err := os.Stat(path)
	return err == nil && info.IsDir()
}

func writeBacklog(sources []tailSource, lines int, offsets logtail.InitialOffsets, write func(logtail.Record) error) error {
	groups := make([][]logtail.Record, 0, len(sources))
	for _, src := range sources {
		text, err := readBacklog(src, lines, offsets)
		if err != nil {
			return fmt.Errorf("read %s log: %w", src.Name, err)
		}
		// Each pass gets its own parser: it is stateful, and the follower holds
		// a separate one for the same source.
		parser := logtail.NewAgentParser(src.Name, src.Seed)
		var group []logtail.Record
		for _, line := range strings.Split(text, "\n") {
			if line == "" {
				continue
			}
			if src.Agent {
				group = append(group, parser.Parse(line))
				continue
			}
			group = append(group, logtail.Unwrap(logtail.ParseLine(src.Name, line)))
		}
		groups = append(groups, group)
	}
	for _, rec := range logtail.Merge(groups) {
		if err := write(rec); err != nil {
			return err
		}
	}
	return nil
}

// readBacklog returns the physical lines a source contributes to the backlog.
// Agent logs are never rotated, so they read straight from their own path.
func readBacklog(src tailSource, lines int, offsets logtail.InitialOffsets) (string, error) {
	if !src.Agent {
		if offsets == nil {
			return bossalog.TailRotating(src.Name, lines)
		}
		offset := offsets[src.Name]
		return bossalog.TailRotatingHandoff(src.Name, lines, offset.Offset, offset.Info, offset.PartialLine)
	}
	if offsets == nil {
		return bossalog.Tail(src.Path, lines)
	}
	offset := offsets[src.Name]
	limit := lines
	if offset.PartialLine {
		// Read one extra physical line so dropping the partial below does not
		// shrink the requested backlog.
		limit++
	}
	text, err := bossalog.TailAt(src.Path, limit, offset.Offset)
	if err != nil || !offset.PartialLine {
		return text, err
	}
	// The snapshot ended inside a line that the follower still holds; emitting
	// it here too would print the record twice.
	if end := strings.LastIndex(text, "\n"); end >= 0 {
		return text[:end], nil
	}
	return "", nil
}

func followSources(ctx context.Context, tailSources []tailSource, lines int, write func(logtail.Record) error, afterReady func()) error {
	if ctx == nil {
		ctx = context.Background()
	}
	ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()
	sources := make([]logtail.Source, 0, len(tailSources))
	for _, src := range tailSources {
		src := src
		source := logtail.Source{Service: src.Name, Path: src.Path}
		if src.Agent {
			// Owned by this follower alone; the backlog pass parses with its own.
			source.Parse = logtail.NewAgentParser(src.Name, src.Seed).Parse
		} else {
			source.SnapshotBackups = func() ([]os.FileInfo, error) {
				return bossalog.BackupInfos(src.Name)
			}
		}
		sources = append(sources, source)
	}
	records, errs := make(chan logtail.Record, 128), make(chan error, 1)
	ready := make(chan logtail.InitialOffsets, 1)
	resume := make(chan struct{})
	go func() { errs <- logtail.FollowFromEndReadyPaused(ctx, sources, records, ready, resume) }()
	select {
	case offsets := <-ready:
		if ctx.Err() != nil {
			return nil
		}
		if afterReady != nil {
			afterReady()
		}
		err := writeBacklog(tailSources, lines, offsets, write)
		close(resume)
		if err != nil {
			return err
		}
	case <-ctx.Done():
		return nil
	case err := <-errs:
		if errors.Is(err, context.Canceled) {
			return nil
		}
		return err
	}
	return writeLiveRecords(ctx, tailSources, records, errs, write)
}

// writeLiveRecords gives concurrently polled sources a short chance to report
// together, then merges that batch with the same ordering used for the initial
// backlog. A single source retains tail's immediate write behavior.
func writeLiveRecords(ctx context.Context, sources []tailSource, records <-chan logtail.Record, errs <-chan error, write func(logtail.Record) error) error {
	if len(sources) < 2 {
		for {
			select {
			case rec := <-records:
				if err := write(rec); err != nil {
					return err
				}
			case <-ctx.Done():
				return nil
			case err := <-errs:
				if errors.Is(err, context.Canceled) {
					return nil
				}
				return err
			}
		}
	}

	groups := make([][]logtail.Record, len(sources))
	groupIndex := make(map[string]int, len(sources))
	for index, src := range sources {
		groupIndex[src.Name] = index
	}
	flush := func() error {
		for _, rec := range logtail.Merge(groups) {
			if err := write(rec); err != nil {
				return err
			}
		}
		for index := range groups {
			groups[index] = groups[index][:0]
		}
		return nil
	}
	appendRecord := func(rec logtail.Record) {
		groups[groupIndex[rec.Service]] = append(groups[groupIndex[rec.Service]], rec)
	}

	for {
		select {
		case rec := <-records:
			appendRecord(rec)
			timer := time.NewTimer(liveMergeWindow)
			var followErr error
		collect:
			for {
				select {
				case rec := <-records:
					appendRecord(rec)
				case <-timer.C:
					break collect
				case <-ctx.Done():
					timer.Stop()
					return nil
				case followErr = <-errs:
					timer.Stop()
					break collect
				}
			}
			if err := flush(); err != nil {
				return err
			}
			if followErr != nil {
				if errors.Is(followErr, context.Canceled) {
					return nil
				}
				return followErr
			}
		case <-ctx.Done():
			return nil
		case err := <-errs:
			if errors.Is(err, context.Canceled) {
				return nil
			}
			return err
		}
	}
}

func quietOnClosedPipe(err error) error {
	if err == nil || errors.Is(err, syscall.EPIPE) || errors.Is(err, os.ErrClosed) || errors.Is(err, io.ErrClosedPipe) {
		return nil
	}
	return err
}
