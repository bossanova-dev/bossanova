package logtail

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"strings"
	"sync"
	"syscall"
	"time"
)

const pollInterval = 100 * time.Millisecond

// Source identifies one service log file to follow.
type Source struct {
	Service, Path   string
	SnapshotBackups func() ([]os.FileInfo, error)
}

// InitialOffset records a source's initial byte length, file identity, backup
// identities, and whether its snapshot ended inside a partial line. A negative
// offset means that the source did not exist at that point.
type InitialOffset struct {
	Offset      int64
	Info        os.FileInfo
	PartialLine bool
	BackupInfos []os.FileInfo
}

// InitialOffsets records each source's state when a follower starts.
type InitialOffsets map[string]InitialOffset

// Follow streams complete source-log lines until ctx ends. It reopens rotated
// files, recovers from truncation, and holds a partial final line until newline.
func Follow(ctx context.Context, sources []Source, out chan<- Record) error {
	return follow(ctx, sources, out, false, nil, nil)
}

// FollowFromEnd streams new complete source-log lines until ctx ends. It starts
// at the end of each source that exists when following begins, then reads any
// rotated replacement files from their beginning.
func FollowFromEnd(ctx context.Context, sources []Source, out chan<- Record) error {
	return follow(ctx, sources, out, true, nil, nil)
}

// FollowFromEndReady streams new complete source-log lines until ctx ends. It
// sends ready after it has captured each source's initial end offset (or
// observed that the source does not yet exist). A caller can use the offsets to
// read a backlog without overlapping records that the follower will emit.
func FollowFromEndReady(ctx context.Context, sources []Source, out chan<- Record, ready chan<- InitialOffsets) error {
	return follow(ctx, sources, out, true, ready, nil)
}

// FollowFromEndReadyPaused is FollowFromEndReady with a startup barrier. Each
// follower captures its initial offset, signals ready, then waits for resume
// before reading a replacement pathname. Callers can flush an exact handoff
// backlog while no replacement records are emitted.
func FollowFromEndReadyPaused(ctx context.Context, sources []Source, out chan<- Record, ready chan<- InitialOffsets, resume <-chan struct{}) error {
	return follow(ctx, sources, out, true, ready, resume)
}

func follow(ctx context.Context, sources []Source, out chan<- Record, seekEnd bool, ready chan<- InitialOffsets, resume <-chan struct{}) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	errs := make(chan error, len(sources))
	var wg sync.WaitGroup
	var initial sync.WaitGroup
	offsets := make(InitialOffsets, len(sources))
	var offsetsMu sync.Mutex
	if ready != nil {
		initial.Add(len(sources))
		go func() {
			initial.Wait()
			select {
			case ready <- offsets:
			case <-ctx.Done():
			}
		}()
	}
	for _, src := range sources {
		wg.Add(1)
		go func(src Source) {
			defer wg.Done()
			var initialized func(InitialOffset)
			if ready != nil {
				initialized = func(initialOffset InitialOffset) {
					offsetsMu.Lock()
					offsets[src.Service] = initialOffset
					offsetsMu.Unlock()
					initial.Done()
				}
			}
			if err := followOne(ctx, src, out, seekEnd, initialized, resume); err != nil {
				errs <- err
				cancel()
			}
		}(src)
	}
	wg.Wait()
	select {
	case err := <-errs:
		return err
	default:
	}
	return ctx.Err()
}

func followOne(ctx context.Context, src Source, out chan<- Record, seekEnd bool, initialized func(InitialOffset), resume <-chan struct{}) error {
	var file *os.File
	var offset int64
	var pending []byte
	var once sync.Once
	markInitialized := func(initialOffset InitialOffset) {
		if initialized != nil {
			once.Do(func() { initialized(initialOffset) })
		}
	}
	waitForResume := func() bool {
		if resume == nil {
			return true
		}
		select {
		case <-resume:
			return true
		case <-ctx.Done():
			return false
		}
	}
	defer func() { markInitialized(InitialOffset{Offset: -1}) }()
	defer func() {
		if file != nil {
			_ = file.Close()
		}
	}()
	for {
		select {
		case <-ctx.Done():
			return nil
		default:
		}
		if file == nil {
			opened, err := openNoFollow(src.Path)
			if err != nil {
				if errors.Is(err, os.ErrNotExist) {
					// The file did not exist when following began, so read its
					// initial contents when it is eventually created.
					seekEnd = false
					backupInfos, err := snapshotBackups(src)
					if err != nil {
						return err
					}
					markInitialized(InitialOffset{Offset: -1, BackupInfos: backupInfos})
					if !waitForResume() {
						return nil
					}
					if sleepOrDone(ctx) {
						return nil
					}
					continue
				}
				return err
			}
			offset = 0
			if seekEnd {
				info, err := opened.Stat()
				if err != nil {
					_ = opened.Close()
					if sleepOrDone(ctx) {
						return nil
					}
					continue
				}
				offset = info.Size()
				pending, err = trailingPartialLine(opened, offset)
				if err != nil {
					_ = opened.Close()
					if sleepOrDone(ctx) {
						return nil
					}
					continue
				}
				seekEnd = false
			} else {
				pending = nil
			}
			file = opened
			info, err := opened.Stat()
			if err != nil {
				_ = opened.Close()
				if sleepOrDone(ctx) {
					return nil
				}
				continue
			}
			backupInfos, err := snapshotBackups(src)
			if err != nil {
				_ = opened.Close()
				return err
			}
			markInitialized(InitialOffset{Offset: offset, Info: info, PartialLine: len(pending) > 0, BackupInfos: backupInfos})
			if !waitForResume() {
				return nil
			}
		}
		rotated, size := rotationState(src.Path, file)
		if size < offset {
			offset, pending = 0, nil
		}
		chunk, read, err := readFrom(file, offset)
		if err != nil && !errors.Is(err, io.EOF) {
			_ = file.Close()
			file = nil
			if sleepOrDone(ctx) {
				return nil
			}
			continue
		}
		offset += read
		pending = append(pending, chunk...)
		complete, remainder := splitCompleteLines(pending)
		pending = remainder
		for _, line := range complete {
			select {
			case out <- Unwrap(ParseLine(src.Service, line)):
			case <-ctx.Done():
				return nil
			}
		}
		if rotated {
			_ = file.Close()
			file = nil
			continue
		}
		if len(complete) == 0 && sleepOrDone(ctx) {
			return nil
		}
	}
}

func snapshotBackups(src Source) ([]os.FileInfo, error) {
	if src.SnapshotBackups == nil {
		return nil, nil
	}
	return src.SnapshotBackups()
}

// trailingPartialLine returns the final unterminated line ending at end. A
// follower started at end keeps that fragment so a later append can complete
// the same record instead of emitting only its suffix.
func trailingPartialLine(file *os.File, end int64) ([]byte, error) {
	const chunkSize = 64 * 1024
	var suffix []byte
	for end > 0 {
		start := max(int64(0), end-chunkSize)
		chunk := make([]byte, end-start)
		n, err := file.ReadAt(chunk, start)
		if err != nil && !errors.Is(err, io.EOF) {
			return nil, err
		}
		chunk = chunk[:n]
		if index := bytes.LastIndexByte(chunk, '\n'); index >= 0 {
			return append(append([]byte(nil), chunk[index+1:]...), suffix...), nil
		}
		suffix = append(chunk, suffix...)
		end = start
	}
	return suffix, nil
}

func rotationState(path string, file *os.File) (bool, int64) {
	openInfo, err := file.Stat()
	if err != nil {
		return true, 0
	}
	diskInfo, err := os.Stat(path)
	if err != nil {
		return false, openInfo.Size()
	}
	return !os.SameFile(diskInfo, openInfo), openInfo.Size()
}

func readFrom(file *os.File, offset int64) ([]byte, int64, error) {
	info, err := file.Stat()
	if err != nil {
		return nil, 0, err
	}
	if info.Size() <= offset {
		return nil, 0, nil
	}
	buf := make([]byte, info.Size()-offset)
	n, err := file.ReadAt(buf, offset)
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, 0, err
	}
	return buf[:n], int64(n), nil
}

func splitCompleteLines(buf []byte) ([]string, []byte) {
	index := bytes.LastIndexByte(buf, '\n')
	if index < 0 {
		return nil, buf
	}
	head := string(buf[:index])
	remainder := append([]byte(nil), buf[index+1:]...)
	var lines []string
	for _, line := range strings.Split(head, "\n") {
		if line != "" {
			lines = append(lines, line)
		}
	}
	return lines, remainder
}

func openNoFollow(path string) (*os.File, error) {
	// #nosec G304 -- path derives from a fixed state dir plus known services; O_NOFOLLOW refuses a symlink at the final component.
	// owner=@recurser review-by=2027-02-05 issue=BOS-707
	return os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
}

func sleepOrDone(ctx context.Context) bool {
	select {
	case <-ctx.Done():
		return true
	case <-time.After(pollInterval):
		return false
	}
}
