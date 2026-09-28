package main

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	bossskillinstall "github.com/recurser/boss/internal/skillinstall"
	"github.com/recurser/bossalib/config"
	libskillinstall "github.com/recurser/bossalib/skillinstall"
)

const staleInstalledSkill = "installed by a different payload\n"

func stubSkillPayloadBuildInfo(t *testing.T, commit, version string) {
	t.Helper()
	orig := skillPayloadBuildInfo
	t.Cleanup(func() { skillPayloadBuildInfo = orig })
	skillPayloadBuildInfo = func() (string, string) { return commit, version }
}

func gitOutput(t *testing.T, root string, args ...string) string {
	t.Helper()
	out, err := exec.Command("git", append([]string{"-C", root}, args...)...).Output()
	if err != nil {
		t.Fatalf("git %v: %v", args, err)
	}
	return strings.TrimSpace(string(out))
}

// setupOrderedCheckout builds a trusted checkout with two commits and enters
// it, WITHOUT trusting it as a payload: the startup payload stays the embedded
// one, and the checkout serves only as the ancestry reference.
func setupOrderedCheckout(t *testing.T) (first, second string) {
	t.Helper()
	root := t.TempDir()
	srcRoot := writeSkillSources(t, root, gateSkillFS())
	t.Setenv(trustCheckoutSkillSourcesEnv, "")
	commit := func(msg string) string {
		runGit(t, root, "add", ".")
		runGit(t, root, "-c", "user.name=Test User", "-c", "user.email=test@example.com", "commit", "--quiet", "-m", msg)
		return gitOutput(t, root, "rev-parse", "HEAD")
	}
	first = commit("first")
	if err := os.WriteFile(filepath.Join(srcRoot, "skills", "boss", "SKILL.md"), []byte("boss skill, second revision\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	second = commit("second")
	t.Chdir(root)
	return first, second
}

// seedStampedCodexTree installs the embedded payload for codex, stamps it with
// record (nil: a legacy unstamped tree), and makes one file differ so any
// refresh decision is actually exercised.
func seedStampedCodexTree(t *testing.T, home string, record *libskillinstall.PayloadRecord) (dir, probe string) {
	t.Helper()
	dir = filepath.Join(home, ".codex", "skills")
	var err error
	if record == nil {
		err = libskillinstall.Extract(dir, bossskillinstall.SkillsFS)
	} else {
		err = libskillinstall.ExtractRecorded(dir, bossskillinstall.SkillsFS, *record)
	}
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	probe = filepath.Join(dir, libskillinstall.Namespace, "boss", "SKILL.md")
	if err := os.WriteFile(probe, []byte(staleInstalledSkill), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir, probe
}

func runHeadlessSelfHeal(t *testing.T) string {
	t.Helper()
	setAvailableSkillAgents(map[string]bool{"codex": true})
	skillInstallIsTerminal = func() bool { return false }
	skillInstallReadAnswer = func() string {
		t.Fatal("self-heal must not prompt")
		return ""
	}
	return captureStderr(t, func() {
		if err := maybeInstallSkills(); err != nil {
			t.Fatalf("maybeInstallSkills: %v", err)
		}
	})
}

func readProbe(t *testing.T, probe string) string {
	t.Helper()
	data, err := os.ReadFile(probe)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func mustPayloadRecord(t *testing.T, dir string) (libskillinstall.PayloadRecord, bool) {
	t.Helper()
	record, recorded, err := libskillinstall.ReadPayloadRecord(dir)
	if err != nil {
		t.Fatalf("ReadPayloadRecord: %v", err)
	}
	return record, recorded
}

// TestSelfHealNoDowngradeHoldsOverCheckoutStampedTree is the observed
// restorer: a peer session's older boss, outside any checkout, runs a headless
// command minutes after a trusted reinstall. It must hold, say so on stderr,
// and record nothing in settings.
func TestSelfHealNoDowngradeHoldsOverCheckoutStampedTree(t *testing.T) {
	home := setupSkillStartupTest(t)
	t.Chdir(t.TempDir())
	stubSkillPayloadBuildInfo(t, "abc1234", "v1.0.0-5-gabc1234")
	checkout := libskillinstall.PayloadRecord{Origin: libskillinstall.OriginCheckout, Revision: "0123456789abcdef0123456789abcdef01234567", Writer: libskillinstall.WriterExplicit}
	dir, probe := seedStampedCodexTree(t, home, &checkout)

	stderr := runHeadlessSelfHeal(t)
	if got := readProbe(t, probe); got != staleInstalledSkill {
		t.Fatalf("held refresh rewrote the installed tree: %q", got)
	}
	for _, want := range []string{"held Codex skill refresh", checkout.Revision, "abc1234", "boss skills install"} {
		if !strings.Contains(stderr, want) {
			t.Fatalf("stderr = %q, want it to contain %q", stderr, want)
		}
	}
	if record, _ := mustPayloadRecord(t, dir); record != checkout {
		t.Fatalf("record = %+v, want the checkout record untouched", record)
	}
	settings, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	if got := settings.SkillsInstalledManifestByAgent["codex"]; got != "" {
		t.Fatalf("held refresh recorded an installed manifest %q", got)
	}
}

func TestSelfHealNoDowngradeRefreshesDescendantPayload(t *testing.T) {
	home := setupSkillStartupTest(t)
	first, second := setupOrderedCheckout(t)
	stubSkillPayloadBuildInfo(t, second[:9], "v1.0.0-2-g"+second[:9])
	installed := libskillinstall.PayloadRecord{Origin: libskillinstall.OriginCheckout, Revision: first, Writer: libskillinstall.WriterExplicit}
	dir, probe := seedStampedCodexTree(t, home, &installed)

	stderr := runHeadlessSelfHeal(t)
	if got := readProbe(t, probe); got == staleInstalledSkill {
		t.Fatalf("descendant payload did not refresh; stderr = %q", stderr)
	}
	record, recorded := mustPayloadRecord(t, dir)
	if !recorded || record.Revision != second[:9] || record.Writer != libskillinstall.WriterUnattended || record.Origin != libskillinstall.OriginEmbedded {
		t.Fatalf("record = %+v (recorded %t), want the unattended embedded payload", record, recorded)
	}
}

func TestSelfHealNoDowngradeHoldsProvablyOlderPayload(t *testing.T) {
	home := setupSkillStartupTest(t)
	first, second := setupOrderedCheckout(t)
	stubSkillPayloadBuildInfo(t, first[:9], "v1.0.0-1-g"+first[:9])
	// Even an unattended record is protected from a provably older payload.
	installed := libskillinstall.PayloadRecord{Origin: libskillinstall.OriginEmbedded, Revision: second[:9], Version: "v1.0.0-2-g" + second[:9], Writer: libskillinstall.WriterUnattended}
	_, probe := seedStampedCodexTree(t, home, &installed)

	stderr := runHeadlessSelfHeal(t)
	if got := readProbe(t, probe); got != staleInstalledSkill {
		t.Fatal("provably older payload overwrote the tree")
	}
	if !strings.Contains(stderr, "payload is older than the installed payload") {
		t.Fatalf("stderr = %q, want the older-payload hold reason", stderr)
	}
}

func TestSelfHealNoDowngradeRefreshesAndStampsLegacyTree(t *testing.T) {
	home := setupSkillStartupTest(t)
	t.Chdir(t.TempDir())
	stubSkillPayloadBuildInfo(t, "abc1234", "v1.0.0-5-gabc1234")
	dir, probe := seedStampedCodexTree(t, home, nil)

	runHeadlessSelfHeal(t)
	if got := readProbe(t, probe); got == staleInstalledSkill {
		t.Fatal("legacy tree was not refreshed")
	}
	record, recorded := mustPayloadRecord(t, dir)
	if !recorded || record.Revision != "abc1234" || record.Writer != libskillinstall.WriterUnattended {
		t.Fatalf("record = %+v (recorded %t), want the refresh to stamp the tree", record, recorded)
	}
}

// A trusted-checkout self-heal is explicit: it keeps its overwrite and claims
// the tree for the checkout, even over a newer-looking record.
func TestSelfHealNoDowngradeTrustedCheckoutOverwritesAndStampsExplicit(t *testing.T) {
	home := setupSkillStartupTest(t)
	_, second := setupOrderedCheckout(t)
	t.Setenv(trustCheckoutSkillSourcesEnv, "1")
	installed := libskillinstall.PayloadRecord{Origin: libskillinstall.OriginEmbedded, Revision: "fffffff", Version: "v9.9.9", Writer: libskillinstall.WriterExplicit}
	dir := filepath.Join(home, ".codex", "skills")
	if err := libskillinstall.ExtractRecorded(dir, gateSkillFS(), installed); err != nil {
		t.Fatal(err)
	}

	runHeadlessSelfHeal(t)
	record, recorded := mustPayloadRecord(t, dir)
	if !recorded || record.Origin != libskillinstall.OriginCheckout || record.Writer != libskillinstall.WriterExplicit {
		t.Fatalf("record = %+v (recorded %t), want an explicit checkout stamp", record, recorded)
	}
	if record.Revision != second || record.Dirty {
		t.Fatalf("record = %+v, want the clean checkout HEAD %s", record, second)
	}
}

// TestMaybeInstallSkillsNoDowngradeDoesNotOfferOlderPayload: a stale TUI must
// not label a provable downgrade "Update".
func TestMaybeInstallSkillsNoDowngradeDoesNotOfferOlderPayload(t *testing.T) {
	home := setupSkillStartupTest(t)
	first, second := setupOrderedCheckout(t)
	stubSkillPayloadBuildInfo(t, first[:9], "v1.0.0-1-g"+first[:9])
	installed := libskillinstall.PayloadRecord{Origin: libskillinstall.OriginCheckout, Revision: second, Writer: libskillinstall.WriterExplicit}
	_, probe := seedStampedCodexTree(t, home, &installed)
	setAvailableSkillAgents(map[string]bool{"codex": true})
	calls := setSkillPromptAnswers(t)

	stderr := captureStderr(t, func() {
		if err := maybeInstallSkills(); err != nil {
			t.Fatalf("maybeInstallSkills: %v", err)
		}
	})
	if *calls != 0 {
		t.Fatalf("prompts = %d, want 0 for a provably older payload", *calls)
	}
	if strings.Contains(stderr, "Update boss skills") {
		t.Fatalf("stderr = %q, offered a downgrade as an Update", stderr)
	}
	if got := readProbe(t, probe); got != staleInstalledSkill {
		t.Fatal("older payload was written")
	}
}

func TestMaybeInstallSkillsNoDowngradeAcceptStampsExplicit(t *testing.T) {
	home := setupSkillStartupTest(t)
	t.Chdir(t.TempDir())
	stubSkillPayloadBuildInfo(t, "abc1234", "v1.0.0-5-gabc1234")
	dir, _ := seedStampedCodexTree(t, home, nil)
	setAvailableSkillAgents(map[string]bool{"codex": true})
	setSkillPromptAnswers(t, "")

	captureStderr(t, func() {
		if err := maybeInstallSkills(); err != nil {
			t.Fatalf("maybeInstallSkills: %v", err)
		}
	})
	record, recorded := mustPayloadRecord(t, dir)
	if !recorded || record.Writer != libskillinstall.WriterExplicit || record.Revision != "abc1234" {
		t.Fatalf("record = %+v (recorded %t), want an explicit stamp from the accepted prompt", record, recorded)
	}
}

// TestSkillSyncWritesPayloadRecordOnEveryExplicitPath covers install (fresh
// and over a stale tree), install --force and sync: each is an operator
// command, so each overwrites and stamps an explicit record — even when a
// newer-looking record would make an unattended writer hold.
func TestSkillSyncWritesPayloadRecordOnEveryExplicitPath(t *testing.T) {
	protected := libskillinstall.PayloadRecord{Origin: libskillinstall.OriginCheckout, Revision: "0123456789abcdef0123456789abcdef01234567", Writer: libskillinstall.WriterExplicit}
	cases := []struct {
		name string
		mode skillSyncMode
		seed *libskillinstall.PayloadRecord
		// fresh leaves the target empty.
		fresh bool
	}{
		{name: "install fresh", mode: skillSyncInstall, fresh: true},
		{name: "install over a stamped stale tree", mode: skillSyncInstall, seed: &protected},
		{name: "install --force", mode: skillSyncForce, seed: &protected},
		{name: "sync", mode: skillSyncUpdateOnly, seed: &protected},
		{name: "sync over a current legacy tree", mode: skillSyncUpdateOnly},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			home := setupSkillStartupTest(t)
			t.Chdir(t.TempDir())
			stubSkillPayloadBuildInfo(t, "abc1234", "dev")
			setAvailableSkillAgents(map[string]bool{"codex": true})
			dir := filepath.Join(home, ".codex", "skills")
			switch {
			case tc.fresh:
			case tc.seed != nil:
				seedStampedCodexTree(t, home, tc.seed)
			default:
				if err := libskillinstall.Extract(dir, bossskillinstall.SkillsFS); err != nil {
					t.Fatal(err)
				}
			}
			var out bytes.Buffer
			if err := runSkillSync(&out, tc.mode, "codex"); err != nil {
				t.Fatalf("runSkillSync: %v\n%s", err, out.String())
			}
			record, recorded := mustPayloadRecord(t, dir)
			want := libskillinstall.PayloadRecord{Origin: libskillinstall.OriginEmbedded, Revision: "abc1234", Version: "dev", Writer: libskillinstall.WriterExplicit}
			if !recorded || record != want {
				t.Fatalf("record = %+v (recorded %t), want %+v", record, recorded, want)
			}
			assertAgentSkillsInstalled(t, dir)
		})
	}
}
