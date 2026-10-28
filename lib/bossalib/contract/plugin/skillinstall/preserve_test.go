package skillinstall

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"testing/fstest"
)

func TestExtractRecordedPreserving(t *testing.T) {
	payload := fstest.MapFS{"skills/boss/SKILL.md": {Data: []byte("payload")}}
	t.Run("preserves drift and legacy directories without following symlinks", func(t *testing.T) {
		dir := filepath.Join(t.TempDir(), "skills")
		if err := Extract(dir, payload); err != nil {
			t.Fatal(err)
		}
		ns := filepath.Join(dir, Namespace)
		write := func(rel, data string, mode os.FileMode) {
			t.Helper()
			p := filepath.Join(dir, rel)
			if err := os.MkdirAll(filepath.Dir(p), 0755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(p, []byte(data), mode); err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(p, mode); err != nil {
				t.Fatal(err)
			}
		}
		write(Namespace+"/boss/SKILL.md", "never committed", 0751)
		write(Namespace+"/boss/extra", "unexpected", 0640)
		write(Namespace+"/boss-retired/notes", "stale skill", 0600)
		if err := os.Symlink("../boss/SKILL.md", filepath.Join(ns, "boss-retired", "link")); err != nil {
			t.Fatal(err)
		}
		if err := os.Remove(filepath.Join(dir, "boss")); err != nil {
			t.Fatal(err)
		}
		write("boss/local", "legacy directory", 0644)
		backup := filepath.Join(t.TempDir(), "backup")
		preserved, err := ExtractRecordedPreserving(dir, payload, PayloadRecord{}, backup)
		if err != nil {
			t.Fatal(err)
		}
		if len(preserved) != 3 {
			t.Fatalf("preserved=%v, want three non-overlapping paths", preserved)
		}
		for rel, want := range map[string]string{Namespace + "/boss/SKILL.md": "never committed", Namespace + "/boss/extra": "unexpected", Namespace + "/boss-retired/notes": "stale skill", "boss/local": "legacy directory"} {
			got, err := os.ReadFile(filepath.Join(backup, rel))
			if err != nil || string(got) != want {
				t.Fatalf("backup %s=%q, %v", rel, got, err)
			}
		}
		info, err := os.Stat(filepath.Join(backup, Namespace, "boss", "SKILL.md"))
		if err != nil || info.Mode().Perm() != 0751 {
			t.Fatalf("mode=%v, %v", info, err)
		}
		target, err := os.Readlink(filepath.Join(backup, Namespace, "boss-retired", "link"))
		if err != nil || target != "../boss/SKILL.md" {
			t.Fatalf("symlink=%q, %v", target, err)
		}
		report, err := SourceDriftReportFS(dir, payload)
		if err != nil || len(report.Entries) != 0 {
			t.Fatalf("drift=%+v, %v", report, err)
		}
	})
	t.Run("preserves namespace root files and colliding legacy names", func(t *testing.T) {
		dir := filepath.Join(t.TempDir(), "skills")
		if err := Extract(dir, payload); err != nil {
			t.Fatal(err)
		}
		files := map[string]string{
			Namespace + "/notes.txt": "namespace notes",
			Namespace + "/boss-old":  "namespace legacy notes",
			"boss-old":               "top-level legacy notes",
		}
		for rel, data := range files {
			if err := os.WriteFile(filepath.Join(dir, rel), []byte(data), 0600); err != nil {
				t.Fatal(err)
			}
		}
		backup := filepath.Join(t.TempDir(), "backup")
		if _, err := ExtractRecordedPreserving(dir, payload, PayloadRecord{}, backup); err != nil {
			t.Fatal(err)
		}
		for rel, want := range files {
			got, err := os.ReadFile(filepath.Join(backup, rel))
			if err != nil || string(got) != want {
				t.Fatalf("backup %s=%q, %v; want %q", rel, got, err, want)
			}
		}
		report, err := SourceDriftReportFS(dir, payload)
		if err != nil || len(report.Entries) != 0 {
			t.Fatalf("drift=%+v, %v", report, err)
		}
	})

	t.Run("copy failure leaves installed tree intact", func(t *testing.T) {
		dir := filepath.Join(t.TempDir(), "skills")
		if err := Extract(dir, payload); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, Namespace, "boss", "SKILL.md"), []byte("unique"), 0600); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, Namespace, "boss", "extra"), []byte("second unique path"), 0600); err != nil {
			t.Fatal(err)
		}
		before := treeFingerprint(t, dir)
		injected := errors.New("copy failed")
		copies := 0
		_, err := extractRecordedPreserving(dir, payload, PayloadRecord{}, filepath.Join(t.TempDir(), "backup"), func(src, dst string) error {
			copies++
			if copies == 2 {
				return injected
			}
			return copyPreservedPath(src, dst)
		})
		if !errors.Is(err, injected) {
			t.Fatalf("error=%v", err)
		}
		if after := treeFingerprint(t, dir); before != after {
			t.Fatalf("installed tree changed:\nbefore %s\nafter %s", before, after)
		}
	})
	t.Run("current and absent trees create no backup", func(t *testing.T) {
		for _, installed := range []bool{false, true} {
			dir := filepath.Join(t.TempDir(), "skills")
			if installed {
				if err := Extract(dir, payload); err != nil {
					t.Fatal(err)
				}
			}
			backup := filepath.Join(t.TempDir(), "backup")
			preserved, err := ExtractRecordedPreserving(dir, payload, PayloadRecord{}, backup)
			if err != nil || len(preserved) != 0 {
				t.Fatalf("preserved=%v error=%v", preserved, err)
			}
			if _, err := os.Stat(backup); !os.IsNotExist(err) {
				t.Fatalf("backup exists: %v", err)
			}
			report, err := SourceDriftReportFS(dir, payload)
			if err != nil || len(report.Entries) != 0 {
				t.Fatalf("drift=%+v %v", report, err)
			}
		}
	})
}
