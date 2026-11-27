package factoryversions

import (
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/recurser/bossalib/contract/plugin/skillinstall"
	"github.com/recurser/bossd/internal/setupcheck"
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

func writePayloadRecordFixture(t *testing.T, dir string, record skillinstall.PayloadRecord) {
	t.Helper()
	skillDir := filepath.Join(dir, skillinstall.Namespace, "boss-build")
	if err := os.MkdirAll(skillDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(skillDir, "SKILL.md"), []byte("installed"), 0o600); err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(record)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".bossanova-skills.payload.json"), data, 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestReadSetupSkills(t *testing.T) {
	running := skillinstall.EmbeddedPayloadRecord("abcdef0123", "v1.198.0", skillinstall.WriterUnattended)
	agents := []skillinstall.Agent{skillinstall.AgentClaude, skillinstall.AgentCodex}

	t.Run("zero skills installed reports SkillsMissing", func(t *testing.T) {
		dirs := map[skillinstall.Agent]string{
			skillinstall.AgentClaude: t.TempDir(),
			skillinstall.AgentCodex:  t.TempDir(),
		}
		got := readSetupSkills(t.Context(), running, agents, func(a skillinstall.Agent) (string, error) { return dirs[a], nil })
		if got.State != setupcheck.SkillsMissing {
			t.Fatalf("State = %v, want SkillsMissing (%+v)", got.State, got)
		}
	})

	t.Run("installed skills older than daemon reports SkillsOutdated", func(t *testing.T) {
		dirs := map[skillinstall.Agent]string{
			skillinstall.AgentClaude: t.TempDir(),
			skillinstall.AgentCodex:  t.TempDir(),
		}
		older := skillinstall.EmbeddedPayloadRecord("1234567abc", "v1.197.0", skillinstall.WriterExplicit)
		writePayloadRecordFixture(t, dirs[skillinstall.AgentClaude], older)

		got := readSetupSkills(t.Context(), running, agents, func(a skillinstall.Agent) (string, error) { return dirs[a], nil })
		if got.State != setupcheck.SkillsOutdated {
			t.Fatalf("State = %v, want SkillsOutdated (%+v)", got.State, got)
		}
		if got.SkillsVersion != "v1.197.0 (1234567abc)" || got.DaemonVersion != "v1.198.0 (abcdef0123)" {
			t.Fatalf("versions = (%q, %q), want (v1.197.0 (1234567abc), v1.198.0 (abcdef0123))", got.SkillsVersion, got.DaemonVersion)
		}
	})

	t.Run("installed skills newer than daemon reports SkillsDaemonOutdated", func(t *testing.T) {
		dirs := map[skillinstall.Agent]string{
			skillinstall.AgentClaude: t.TempDir(),
			skillinstall.AgentCodex:  t.TempDir(),
		}
		newer := skillinstall.EmbeddedPayloadRecord("7654321fed", "v1.199.0", skillinstall.WriterExplicit)
		writePayloadRecordFixture(t, dirs[skillinstall.AgentClaude], newer)

		got := readSetupSkills(t.Context(), running, agents, func(a skillinstall.Agent) (string, error) { return dirs[a], nil })
		if got.State != setupcheck.SkillsDaemonOutdated {
			t.Fatalf("State = %v, want SkillsDaemonOutdated (%+v)", got.State, got)
		}
		if got.SkillsVersion != "v1.199.0 (7654321fed)" || got.DaemonVersion != "v1.198.0 (abcdef0123)" {
			t.Fatalf("versions = (%q, %q), want (v1.199.0 (7654321fed), v1.198.0 (abcdef0123))", got.SkillsVersion, got.DaemonVersion)
		}
	})

	t.Run("matching installed skills reports SkillsCurrent even when other agents are absent", func(t *testing.T) {
		dirs := map[skillinstall.Agent]string{
			skillinstall.AgentClaude: t.TempDir(),
			skillinstall.AgentCodex:  t.TempDir(),
		}
		same := skillinstall.EmbeddedPayloadRecord("abcdef0", "v1.198.0", skillinstall.WriterExplicit)
		writePayloadRecordFixture(t, dirs[skillinstall.AgentClaude], same)

		got := readSetupSkills(t.Context(), running, agents, func(a skillinstall.Agent) (string, error) { return dirs[a], nil })
		if got.State != setupcheck.SkillsCurrent {
			t.Fatalf("State = %v, want SkillsCurrent (%+v)", got.State, got)
		}
	})

	t.Run("same release tag with different commit fails closed as SkillsUnreadable", func(t *testing.T) {
		dirs := map[skillinstall.Agent]string{
			skillinstall.AgentClaude: t.TempDir(),
			skillinstall.AgentCodex:  t.TempDir(),
		}
		retagged := skillinstall.EmbeddedPayloadRecord("9999999aaa", "v1.198.0", skillinstall.WriterExplicit)
		writePayloadRecordFixture(t, dirs[skillinstall.AgentClaude], retagged)

		got := readSetupSkills(t.Context(), running, agents, func(a skillinstall.Agent) (string, error) { return dirs[a], nil })
		if got.State != setupcheck.SkillsUnreadable {
			t.Fatalf("State = %v, want SkillsUnreadable (%+v)", got.State, got)
		}
	})

	t.Run("directory lookup error fails closed as SkillsUnreadable", func(t *testing.T) {
		got := readSetupSkills(t.Context(), running, agents, func(skillinstall.Agent) (string, error) {
			return "", errors.New("no home dir")
		})
		if got.State != setupcheck.SkillsUnreadable {
			t.Fatalf("State = %v, want SkillsUnreadable (%+v)", got.State, got)
		}
	})

	t.Run("checkout git ancestry distinguishes older and newer installed skills", func(t *testing.T) {
		repoDir := t.TempDir()
		git := func(args ...string) string {
			t.Helper()
			cmd := exec.CommandContext(t.Context(), "git", append([]string{"-C", repoDir}, args...)...)
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

		checkoutRec := func(rev string) skillinstall.PayloadRecord {
			return skillinstall.PayloadRecord{Origin: skillinstall.OriginCheckout, Revision: rev, Version: "v1.198.0", SourceRoot: repoDir}
		}

		// Installed skills older than daemon -> SkillsOutdated
		olderDir := t.TempDir()
		writePayloadRecordFixture(t, olderDir, checkoutRec(first))
		got := readSetupSkills(t.Context(), checkoutRec(second), []skillinstall.Agent{skillinstall.AgentClaude}, func(skillinstall.Agent) (string, error) { return olderDir, nil })
		if got.State != setupcheck.SkillsOutdated {
			t.Fatalf("older checkout skills: State = %v, want SkillsOutdated (%+v)", got.State, got)
		}

		// Installed skills newer than daemon -> SkillsDaemonOutdated
		newerDir := t.TempDir()
		writePayloadRecordFixture(t, newerDir, checkoutRec(second))
		got = readSetupSkills(t.Context(), checkoutRec(first), []skillinstall.Agent{skillinstall.AgentClaude}, func(skillinstall.Agent) (string, error) { return newerDir, nil })
		if got.State != setupcheck.SkillsDaemonOutdated {
			t.Fatalf("newer checkout skills: State = %v, want SkillsDaemonOutdated (%+v)", got.State, got)
		}
	})
}
