package accountwiring

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	bossanovav1 "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossd/internal/agent"
	"github.com/recurser/bossd/internal/domain"
	"github.com/rs/zerolog"
)

type probeBudgetWriter struct {
	io.WriteCloser
	remaining *int64
	written   *int64
}

func (w probeBudgetWriter) Write(p []byte) (int, error) {
	if int64(len(p)) > *w.remaining {
		return 0, syscall.ENOSPC
	}
	n, err := w.WriteCloser.Write(p)
	*w.remaining -= int64(n)
	*w.written += int64(n)
	return n, err
}

func TestSeedCodexProbeSessionsBoundedTails_Accounts_AC12(t *testing.T) {
	src, dst := t.TempDir(), t.TempDir()
	expected := map[string]time.Time{}
	now := time.Now().Truncate(time.Second)
	for i := 0; i < 48; i++ {
		// The newest rollout is a resumed session in an older date directory.
		day := "28"
		if i == 47 {
			day = "01"
		}
		rel := filepath.Join("sessions", "2026", "10", day, fmt.Sprintf("rollout-%02d.jsonl", i))
		path := filepath.Join(src, rel)
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, nil, 0o600); err != nil {
			t.Fatal(err)
		}
		size := int64(3 * 1024 * 1024)
		if i == 46 {
			size = 100
		}
		if err := os.Truncate(path, size); err != nil {
			t.Fatal(err)
		}
		f, err := os.OpenFile(path, os.O_WRONLY, 0)
		if err != nil {
			t.Fatal(err)
		}
		tail := []byte(fmt.Sprintf("\n{\"timestamp\":\"2026-10-28T00:00:00Z\",\"type\":\"event_msg\",\"payload\":{\"type\":\"token_count\",\"rate_limits\":{\"primary\":{\"used_percent\":%d}}}}\n", i))
		if size < int64(len(tail)) {
			tail = []byte("{}\n")
		}
		if _, err = f.WriteAt(tail, size-int64(len(tail))); err != nil {
			t.Fatal(err)
		}
		if err = f.Close(); err != nil {
			t.Fatal(err)
		}
		mtime := now.Add(time.Duration(i) * time.Second)
		if err = os.Chtimes(path, mtime, mtime); err != nil {
			t.Fatal(err)
		}
		if i >= 40 {
			expected[rel] = mtime
		}
	}
	budget := int64(1024 * 1024)
	var written int64
	create := func(path string, perm os.FileMode) (io.WriteCloser, error) {
		f, err := os.OpenFile(path, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, perm)
		return probeBudgetWriter{f, &budget, &written}, err
	}
	if err := seedCodexProbeSessions(src, dst, create); err != nil {
		t.Fatal(err)
	}
	count := 0
	var total int64
	err := filepath.WalkDir(dst, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		count++
		rel, err := filepath.Rel(dst, path)
		if err != nil {
			return err
		}
		mtime, ok := expected[rel]
		if !ok {
			t.Errorf("unexpected seed %s", rel)
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		total += info.Size()
		if info.Mode().Perm() != 0o600 || !info.ModTime().Equal(mtime) {
			t.Errorf("metadata %s: %v %v", rel, info.Mode(), info.ModTime())
		}
		original, err := os.ReadFile(filepath.Join(src, rel))
		if err != nil {
			return err
		}
		if len(original) > 64*1024 {
			original = original[len(original)-64*1024:]
		}
		got, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if !bytes.Equal(got, original) {
			t.Errorf("tail differs %s", rel)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if count != 8 || total > 8*64*1024 || written != total {
		t.Fatalf("count=%d bytes=%d written=%d", count, total, written)
	}
}

func TestSeedCodexProbeSessionsSkipsUnreadable_Accounts_AC12(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root can read mode 000 files")
	}
	src, dst := t.TempDir(), t.TempDir()
	sessions := filepath.Join(src, "sessions")
	if err := os.Mkdir(sessions, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"rollout-unreadable.jsonl", "rollout-readable.jsonl"} {
		mode := os.FileMode(0o600)
		if name == "rollout-unreadable.jsonl" {
			mode = 0
		}
		if err := os.WriteFile(filepath.Join(sessions, name), []byte("{}\n"), mode); err != nil {
			t.Fatal(err)
		}
	}
	if err := seedUsageProbeForTest(src, dst); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dst, "sessions", "rollout-readable.jsonl")); err != nil {
		t.Fatal(err)
	}
}
func seedUsageProbeForTest(src, dst string) error {
	return seedCodexProbeSessions(src, dst, func(path string, mode os.FileMode) (io.WriteCloser, error) {
		return os.OpenFile(path, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, mode)
	})
}

func TestProbeUsageSnapshotCleansTemporaryHome_Accounts_AC12(t *testing.T) {
	for _, tc := range []struct {
		name, path string
		probeErr   error
		wantErr    bool
	}{
		{name: "success", path: "auth.json"},
		{name: "probe error", path: "auth.json", probeErr: errors.New("probe failed"), wantErr: true},
		{name: "invalid materialized file", path: "../escape", wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tmp := t.TempDir()
			t.Setenv("TMPDIR", tmp)
			t.Setenv("CODEX_HOME", t.TempDir())
			client := &fakeRotationClient{homeDirKey: "CODEX_HOME", files: []*bossanovav1.MaterializedFile{{RelativePath: tc.path, Content: []byte("credential")}}, probeErr: tc.probeErr, probe: &bossanovav1.RateLimitStatus{}}
			m := NewMaterializer(map[string]agent.RunnerClient{"codex": client}, &spyStore{accounts: map[string]*domain.AgentAccount{"codex-1": newCodexAccount()}}, &fakeCreds{blob: []byte("secret")}, zerolog.Nop())
			_, err := m.(interface {
				ProbeUsageSnapshot(context.Context, string) (domain.UsageSnapshot, error)
			}).ProbeUsageSnapshot(context.Background(), "codex-1")
			if (err != nil) != tc.wantErr {
				t.Fatalf("err=%v wantErr=%v", err, tc.wantErr)
			}
			paths, err := filepath.Glob(filepath.Join(tmp, "boss-usage-probe-*"))
			if err != nil {
				t.Fatal(err)
			}
			if len(paths) != 0 {
				t.Fatalf("orphan homes: %v", paths)
			}
		})
	}
}
