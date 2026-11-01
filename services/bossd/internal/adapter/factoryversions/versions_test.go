package factoryversions

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/recurser/bossalib/contract/plugin/skillinstall"
)

func TestReadRecordLegacyInstall(t *testing.T) {
	dir := t.TempDir()
	if _, found, err := readRecord(dir); found || err != nil {
		t.Fatalf("absent install = %v, %v", found, err)
	}
	skill := filepath.Join(dir, skillinstall.Namespace, "boss-build")
	if err := os.MkdirAll(skill, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(skill, "SKILL.md"), []byte("installed"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := readRecord(dir); err == nil {
		t.Fatal("installed skills without revision metadata must be unreadable")
	}
	if err := os.WriteFile(filepath.Join(dir, ".bossanova-skills.payload.json"), []byte("invalid"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := readRecord(dir); err == nil {
		t.Fatal("malformed revision metadata must be unreadable")
	}
}

func TestCompareRevisions(t *testing.T) {
	dir := t.TempDir()
	git := func(args ...string) string {
		t.Helper()
		cmd := exec.CommandContext(t.Context(), "git", append([]string{"-C", dir}, args...)...)
		cmd.WaitDelay = time.Second
		output, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %s: %v", args, output, err)
		}
		return strings.TrimSpace(string(output))
	}
	git("init")
	git("config", "commit.gpgsign", "false")
	git("config", "user.name", "Test")
	git("config", "user.email", "test@example.com")
	git("commit", "--allow-empty", "-m", "first")
	first := git("rev-parse", "HEAD")
	git("commit", "--allow-empty", "-m", "second")
	second := git("rev-parse", "HEAD")
	record := func(revision string) skillinstall.PayloadRecord {
		return skillinstall.PayloadRecord{Origin: skillinstall.OriginCheckout, Revision: revision, Version: "v1.0.0", SourceRoot: dir}
	}
	for _, tc := range []struct {
		name, installed, running string
		wantOlder, wantError     bool
	}{
		{"newer skills", second, first, true, false},
		{"older skills", first, second, false, false},
		{"same revision", first, first[:7], false, false},
		{"missing revision", "abcdef0123456789", first, false, true},
		{"invalid revision", "--help", first, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			older, err := compare(t.Context(), record(tc.installed), record(tc.running))
			if older != tc.wantOlder || (err != nil) != tc.wantError {
				t.Fatalf("compare = %v, %v; want %v, error %v", older, err, tc.wantOlder, tc.wantError)
			}
		})
	}
}

func TestCompareEmbeddedReleases(t *testing.T) {
	installed := skillinstall.EmbeddedPayloadRecord("abcdef0123", "v1.2.0", skillinstall.WriterExplicit)
	running := skillinstall.EmbeddedPayloadRecord("1234567abc", "v1.1.0", skillinstall.WriterUnattended)
	older, err := compare(t.Context(), installed, running)
	if err != nil || !older {
		t.Fatalf("compare = %v, %v", older, err)
	}
	installed.Dirty = true
	if _, err := compare(t.Context(), installed, running); err == nil {
		t.Fatal("dirty record must not prove version order")
	}
	installed.Dirty = false
	installed.Origin = skillinstall.OriginCheckout
	if _, err := compare(t.Context(), installed, running); err == nil {
		t.Fatal("checkout record without a source repository must not prove version order")
	}
}
