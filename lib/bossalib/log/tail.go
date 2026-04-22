package log

import (
	"bytes"
	"errors"
	"io"
	"io/fs"
	"os"
	"strings"
)

// tailChunkSize is the block size we read from the end of the file for large files.
const tailChunkSize = 64 * 1024

// tailReadAllThreshold is the file-size ceiling below which we just read the whole file.
const tailReadAllThreshold = 1 << 20 // 1 MB

// Tail returns the last maxLines lines of the file at path. A missing file returns
// ("", nil) so callers can treat it as an empty log. maxLines <= 0 returns ("", nil).
func Tail(path string, maxLines int) (string, error) {
	if maxLines <= 0 {
		return "", nil
	}
	f, err := os.Open(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return "", nil
		}
		return "", err
	}
	defer func() { _ = f.Close() }()

	info, err := f.Stat()
	if err != nil {
		return "", err
	}

	size := info.Size()
	if size == 0 {
		return "", nil
	}

	if size <= tailReadAllThreshold {
		data, err := io.ReadAll(f)
		if err != nil {
			return "", err
		}
		return lastLines(data, maxLines), nil
	}

	// Read from the end in chunks until we have enough newlines (or hit the start).
	// The loop usually exits in 1-2 iterations, so a fresh chunk per pass is fine.
	var buf []byte
	offset := size
	needed := maxLines + 1 // a trailing newline yields an empty final line, so ask for one more
	for offset > 0 {
		readSize := min(int64(tailChunkSize), offset)
		offset -= readSize
		chunk := make([]byte, readSize) //nolint:prealloc // fresh buffer for ReadAt; prepended below
		if _, err := f.ReadAt(chunk, offset); err != nil {
			return "", err
		}
		buf = append(chunk, buf...)
		if bytes.Count(buf, []byte{'\n'}) >= needed {
			break
		}
	}
	return lastLines(buf, maxLines), nil
}

// lastLines returns the last n lines of data as a single string, preserving
// the original trailing newline (if any).
func lastLines(data []byte, n int) string {
	s := string(data)
	trimmed := strings.TrimRight(s, "\n")
	if trimmed == "" {
		return ""
	}
	lines := strings.Split(trimmed, "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	out := strings.Join(lines, "\n")
	if strings.HasSuffix(s, "\n") {
		out += "\n"
	}
	return out
}
