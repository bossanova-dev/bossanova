package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/recurser/boss/internal/logtail"
	bossalog "github.com/recurser/bossalib/log"
)

const liveMergeWindow = 10 * time.Millisecond

func tailCmd() *cobra.Command {
	var all, follow, asJSON bool
	var lines int
	var filter logtail.Filter
	cmd := &cobra.Command{
		Use: "tail [source]", Short: "Tail daemon logs",
		Long: "Show recent log lines without needing to know where the log files live.\n\n" +
			"Sources: " + strings.Join(bossalog.Services(), ", ") + " (default bossd).\n" +
			"-n counts physical lines read from each source before filtering. Non-JSON lines always pass filters.",
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			// Go otherwise exits with SIGPIPE before a stdout write can return
			// EPIPE, which makes `boss tail -f | head` report status 141 instead
			// of the conventional clean tail exit. quietOnClosedPipe handles the
			// resulting write error below.
			signal.Ignore(syscall.SIGPIPE)
			sources, err := resolveSources(args, all)
			if err != nil {
				return err
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

func resolveSources(args []string, all bool) ([]string, error) {
	if all {
		if len(args) > 0 {
			return nil, errors.New("pass a source or --all, not both")
		}
		return bossalog.Services(), nil
	}
	if len(args) == 0 {
		return []string{"bossd"}, nil
	}
	if !bossalog.KnownService(args[0]) {
		return nil, fmt.Errorf("unknown log source %q (want one of: %s)", args[0], strings.Join(bossalog.Services(), ", "))
	}
	return args[:1], nil
}

func writeBacklog(sources []string, lines int, offsets logtail.InitialOffsets, write func(logtail.Record) error) error {
	groups := make([][]logtail.Record, 0, len(sources))
	for _, service := range sources {
		var text string
		var err error
		if offsets == nil {
			text, err = bossalog.TailRotating(service, lines)
		} else {
			offset := offsets[service]
			text, err = bossalog.TailRotatingHandoff(service, lines, offset.Offset, offset.Info, offset.PartialLine)
		}
		if err != nil {
			return fmt.Errorf("read %s log: %w", service, err)
		}
		var group []logtail.Record
		lines := strings.Split(text, "\n")
		for _, line := range lines {
			if line != "" {
				group = append(group, logtail.Unwrap(logtail.ParseLine(service, line)))
			}
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

func followSources(ctx context.Context, services []string, lines int, write func(logtail.Record) error, afterReady func()) error {
	if ctx == nil {
		ctx = context.Background()
	}
	ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()
	sources := make([]logtail.Source, 0, len(services))
	for _, service := range services {
		service := service
		sources = append(sources, logtail.Source{
			Service: service,
			Path:    bossalog.LogPath(service),
			SnapshotBackups: func() ([]os.FileInfo, error) {
				return bossalog.BackupInfos(service)
			},
		})
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
		err := writeBacklog(services, lines, offsets, write)
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
	return writeLiveRecords(ctx, services, records, errs, write)
}

// writeLiveRecords gives concurrently polled sources a short chance to report
// together, then merges that batch with the same ordering used for the initial
// backlog. A single source retains tail's immediate write behavior.
func writeLiveRecords(ctx context.Context, services []string, records <-chan logtail.Record, errs <-chan error, write func(logtail.Record) error) error {
	if len(services) < 2 {
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

	groups := make([][]logtail.Record, len(services))
	groupIndex := make(map[string]int, len(services))
	for index, service := range services {
		groupIndex[service] = index
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
