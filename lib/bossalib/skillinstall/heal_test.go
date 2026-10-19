package skillinstall

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"
	"time"
)

func TestHealPathsCompareAndApply(t *testing.T) {
	dir := t.TempDir()
	ns := filepath.Join(dir, Namespace, "boss-build")
	if err := os.MkdirAll(ns, 0o750); err != nil {
		t.Fatal(err)
	}
	write := func(name, data string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(ns, name), []byte(data), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	fp := func(rel string) string {
		t.Helper()
		v, err := HealFingerprint(dir, rel)
		if err != nil {
			t.Fatal(err)
		}
		return v
	}
	write("changed.md", "old")
	old := fp("boss-build/changed.md")
	write("changed.md", "peer")
	write("mode.sh", "echo yes")
	write("remove.md", "old")
	write("keep.md", "keep")
	result, err := HealPaths(dir, []HealFix{
		{Path: "boss-build/changed.md", Operation: HealWrite, Data: []byte("replacement"), ExpectedFingerprint: old},
		{Path: "boss-build/mode.sh", Operation: HealChmod, Data: []byte("echo yes"), ExpectedFingerprint: fp("boss-build/mode.sh")},
		{Path: "boss-build/new", Operation: HealWrite, Data: []byte("#!/bin/sh\n"), ExpectedFingerprint: "absent"},
		{Path: "boss-build/remove.md", Operation: HealRemove, ExpectedFingerprint: fp("boss-build/remove.md")},
		{Path: "boss-build", Operation: HealRelink, ExpectedFingerprint: fp("boss-build")},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Healed) != 4 || len(result.Skipped) != 1 {
		t.Fatalf("result: %+v", result)
	}
	data, err := os.ReadFile(filepath.Join(ns, "changed.md"))
	if err != nil || string(data) != "peer" {
		t.Fatalf("peer changed: %q %v", data, err)
	}
	for _, name := range []string{"mode.sh", "new"} {
		info, err := os.Stat(filepath.Join(ns, name))
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != 0o755 {
			t.Fatalf("%s mode %v", name, info.Mode())
		}
	}
	if _, err := os.Stat(filepath.Join(ns, "remove.md")); !os.IsNotExist(err) {
		t.Fatalf("remove: %v", err)
	}
	if data, err := os.ReadFile(filepath.Join(ns, "keep.md")); err != nil || string(data) != "keep" {
		t.Fatalf("sibling: %q %v", data, err)
	}
	target, err := os.Readlink(filepath.Join(dir, "boss-build"))
	if err != nil || target != filepath.Join(Namespace, "boss-build") {
		t.Fatalf("link: %q %v", target, err)
	}
	entries, err := os.ReadDir(ns)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".boss-heal-") {
			t.Fatalf("temp leaked: %s", entry.Name())
		}
	}
}

func TestHealPathsRejectEscape(t *testing.T) {
	for _, rel := range []string{"../outside", "/boss/file", "boss/../../outside", "boss\\file", "unrelated/file"} {
		t.Run(rel, func(t *testing.T) {
			if _, err := HealPaths(t.TempDir(), []HealFix{{Path: rel, Operation: HealWrite, ExpectedFingerprint: "absent"}}); err == nil {
				t.Fatal("accepted unsafe path")
			}
		})
	}
	dir := t.TempDir()
	outside := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, Namespace), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(dir, Namespace, "boss")); err != nil {
		t.Fatal(err)
	}
	if _, err := HealPaths(dir, []HealFix{{Path: "boss/file", Operation: HealWrite, Data: []byte("x"), ExpectedFingerprint: "absent"}}); err == nil {
		t.Fatal("accepted symlink parent")
	}
	entries, err := os.ReadDir(outside)
	if err != nil || len(entries) != 0 {
		t.Fatalf("outside changed: %v %v", entries, err)
	}
}

func TestHealFingerprintIncludesModeAndLink(t *testing.T) {
	dir := t.TempDir()
	ns := filepath.Join(dir, Namespace, "boss")
	if err := os.MkdirAll(ns, 0o750); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(ns, "x.md")
	if err := os.WriteFile(file, []byte("hello\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	before, err := HealFingerprint(dir, "boss/x.md")
	if err != nil {
		t.Fatal(err)
	}
	if blob := HealFingerprintBlob(before); blob != "ce013625030ba8dba906f756967f9e9ca394464a" {
		t.Fatalf("git blob %q", blob)
	}
	if err := os.Chmod(file, 0o755); err != nil {
		t.Fatal(err)
	}
	result, err := HealPaths(dir, []HealFix{{Path: "boss/x.md", Operation: HealWrite, Data: []byte("new"), ExpectedFingerprint: before}})
	if err != nil || len(result.Skipped) != 1 {
		t.Fatalf("mode race: %+v %v", result, err)
	}
	link := filepath.Join(dir, "boss")
	if err := os.Symlink("old", link); err != nil {
		t.Fatal(err)
	}
	old, err := HealFingerprint(dir, "boss")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(link); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("new", link); err != nil {
		t.Fatal(err)
	}
	result, err = HealPaths(dir, []HealFix{{Path: "boss", Operation: HealRelink, ExpectedFingerprint: old}})
	if err != nil || len(result.Skipped) != 1 {
		t.Fatalf("link race: %+v %v", result, err)
	}
}

func TestHealPathsNeverRecursivelyRemoves(t *testing.T) {
	dir := t.TempDir()
	ns := filepath.Join(dir, Namespace, "boss-old")
	if err := os.MkdirAll(ns, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ns, "keep"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	fp, err := HealFingerprint(dir, "boss-old/")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := HealPaths(dir, []HealFix{{Path: "boss-old/", Operation: HealRemove, ExpectedFingerprint: fp}}); err == nil {
		t.Fatal("removed nonempty directory")
	}
	if _, err := os.Stat(filepath.Join(ns, "keep")); err != nil {
		t.Fatal(err)
	}
}

func TestStampPayloadIfCurrentPreservesDrift(t *testing.T) {
	dir := t.TempDir()
	payload := fstest.MapFS{"skills/boss/SKILL.md": {Data: []byte("payload")}}
	if err := Extract(dir, payload); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(dir, Namespace, "boss", "SKILL.md")
	if err := os.WriteFile(file, []byte("peer"), 0o644); err != nil {
		t.Fatal(err)
	}
	stamped, err := StampPayloadIfCurrent(dir, payload, PayloadRecord{})
	if err != nil || stamped {
		t.Fatalf("stamped drift: %v %v", stamped, err)
	}
	data, err := os.ReadFile(file)
	if err != nil || string(data) != "peer" {
		t.Fatalf("peer overwritten: %q %v", data, err)
	}
}

func TestHealPathsRechecksAfterUpdateLock(t *testing.T) {
	dir := t.TempDir()
	payload := fstest.MapFS{"skills/boss/SKILL.md": {Data: []byte("old")}}
	if err := Extract(dir, payload); err != nil {
		t.Fatal(err)
	}
	before, err := HealFingerprint(dir, "boss/SKILL.md")
	if err != nil {
		t.Fatal(err)
	}
	lock, err := acquireUpdateLock(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = lock.Unlock() }()
	type outcome struct {
		result HealResult
		err    error
	}
	done := make(chan outcome, 1)
	go func() {
		result, err := HealPaths(dir, []HealFix{{Path: "boss/SKILL.md", Operation: HealWrite, Data: []byte("replacement"), ExpectedFingerprint: before}})
		done <- outcome{result, err}
	}()
	select {
	case result := <-done:
		t.Fatalf("heal bypassed held lock: %+v", result)
	case <-time.After(25 * time.Millisecond):
	}
	file := filepath.Join(dir, Namespace, "boss", "SKILL.md")
	if err := os.WriteFile(file, []byte("concurrent writer"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := lock.Unlock(); err != nil {
		t.Fatal(err)
	}
	select {
	case result := <-done:
		if result.err != nil || len(result.result.Skipped) != 1 {
			t.Fatalf("recheck: %+v", result)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("heal did not release lock")
	}
	data, err := os.ReadFile(file)
	if err != nil || string(data) != "concurrent writer" {
		t.Fatalf("peer clobbered: %q %v", data, err)
	}
}

func TestStampPayloadIfCurrentAndFrozenReport(t *testing.T) {
	dir := t.TempDir()
	payload := fstest.MapFS{"skills/boss/SKILL.md": {Data: []byte("payload")}}
	if err := Extract(dir, payload); err != nil {
		t.Fatal(err)
	}
	record := EmbeddedPayloadRecord("abcdef", "1.0.0", WriterUnattended)
	stamped, err := StampPayloadIfCurrent(dir, payload, record)
	if err != nil || !stamped {
		t.Fatalf("stamp: %v %v", stamped, err)
	}
	observed, exists, err := ReadPayloadRecord(dir)
	if err != nil || !exists || observed != record {
		t.Fatalf("record: %+v %v %v", observed, exists, err)
	}
	report, err := SourceDriftReportFS(dir, payload)
	if err != nil || !report.Installed || report.Compared != 1 || len(report.Entries) != 0 {
		t.Fatalf("report: %+v %v", report, err)
	}
}

func TestHealPathsPreservesRealTopLevelEntries(t *testing.T) {
	for _, operation := range []HealOperation{HealRemove, HealRelink} {
		t.Run(string(operation), func(t *testing.T) {
			dir := t.TempDir()
			file := filepath.Join(dir, "boss")
			if err := os.WriteFile(file, []byte("operator content"), 0o644); err != nil {
				t.Fatal(err)
			}
			fingerprint, err := HealFingerprint(dir, "boss")
			if err != nil {
				t.Fatal(err)
			}
			if _, err := HealPaths(dir, []HealFix{{Path: "boss", Operation: operation, ExpectedFingerprint: fingerprint}}); err == nil {
				t.Fatal("accepted real top-level file")
			}
			if data, err := os.ReadFile(file); err != nil || string(data) != "operator content" {
				t.Fatalf("changed top-level file: %q %v", data, err)
			}
		})
	}
}
