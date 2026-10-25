package tuitest

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/recurser/boss/internal/tuidriver"
)

// captureCast registers the file before the driver cleanup so output drains first.
func captureCast(t *testing.T, width, height int) io.Writer {
	t.Helper()
	dir := os.Getenv("TUITEST_CAST_DIR")
	if dir == "" {
		return nil
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("create cast directory: %v", err)
	}
	name := strings.NewReplacer("/", "_", "\\", "_").Replace(t.Name())
	file, err := os.Create(filepath.Join(dir, name+".cast"))
	if err != nil {
		t.Fatalf("create cast: %v", err)
	}
	t.Cleanup(func() {
		if err := file.Close(); err != nil {
			t.Errorf("close cast: %v", err)
		}
	})
	writer, err := tuidriver.NewCastWriter(file, width, height, time.Now)
	if err != nil {
		t.Fatalf("initialize cast: %v", err)
	}
	return writer
}
