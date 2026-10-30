package skills

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	libskillinstall "github.com/recurser/bossalib/contract/plugin/skillinstall"
	"github.com/recurser/bossalib/contract/plugin/skillpayload"
	"github.com/recurser/bossalib/kit/config"
)

const staleInstalledSkill = "installed by a different payload\n"

func (g *Group) stubSkillPayloadBuildInfo(t *testing.T, commit, version string) {
	t.Helper()
	orig := g.skillPayloadBuildInfo
	t.Cleanup(func() { g.skillPayloadBuildInfo = orig })
	g.skillPayloadBuildInfo = func() (string, string) { return commit, version }
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
		err = libskillinstall.Extract(dir, skillpayload.SkillsFS)
	} else {
		err = libskillinstall.ExtractRecorded(dir, skillpayload.SkillsFS, *record)
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

func (g *Group) runHeadlessSelfHeal(t *testing.T) string {
	t.Helper()
	g.setAvailableSkillAgents(map[string]bool{"codex": true})
	g.StartupIsTerminal = func() bool { return false }
	g.StartupReadAnswer = func() string {
		t.Fatal("self-heal must not prompt")
		return ""
	}
	return captureStderr(t, func() {
		if err := g.maybeInstallSkills(); err != nil {
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
// and leave install/decline bookkeeping untouched.
func TestSelfHealNoDowngradeHoldsOverCheckoutStampedTree(t *testing.T) {
	g := newTestGroup()

	home := g.setupSkillStartupTest(t)
	t.Chdir(t.TempDir())
	g.stubSkillPayloadBuildInfo(t, "abc1234", "v1.0.0-5-gabc1234")
	checkout := libskillinstall.PayloadRecord{Origin: libskillinstall.OriginCheckout, Revision: "0123456789abcdef0123456789abcdef01234567", Writer: libskillinstall.WriterExplicit}
	dir, probe := seedStampedCodexTree(t, home, &checkout)

	stderr := g.runHeadlessSelfHeal(t)
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
	g := newTestGroup()

	home := g.setupSkillStartupTest(t)
	first, second := setupOrderedCheckout(t)
	g.stubSkillPayloadBuildInfo(t, second[:9], "v1.0.0-2-g"+second[:9])
	installed := libskillinstall.PayloadRecord{Origin: libskillinstall.OriginCheckout, Revision: first, Writer: libskillinstall.WriterExplicit}
	dir, probe := seedStampedCodexTree(t, home, &installed)

	stderr := g.runHeadlessSelfHeal(t)
	if got := readProbe(t, probe); got == staleInstalledSkill {
		t.Fatalf("descendant payload did not refresh; stderr = %q", stderr)
	}
	record, recorded := mustPayloadRecord(t, dir)
	if !recorded || record.Revision != second[:9] || record.Writer != libskillinstall.WriterUnattended || record.Origin != libskillinstall.OriginEmbedded {
		t.Fatalf("record = %+v (recorded %t), want the unattended embedded payload", record, recorded)
	}
}

func TestSelfHealNoDowngradeHoldsProvablyOlderPayload(t *testing.T) {
	g := newTestGroup()

	home := g.setupSkillStartupTest(t)
	first, second := setupOrderedCheckout(t)
	g.stubSkillPayloadBuildInfo(t, first[:9], "v1.0.0-1-g"+first[:9])
	// Even an unattended record is protected from a provably older payload.
	installed := libskillinstall.PayloadRecord{Origin: libskillinstall.OriginEmbedded, Revision: second[:9], Version: "v1.0.0-2-g" + second[:9], Writer: libskillinstall.WriterUnattended}
	_, probe := seedStampedCodexTree(t, home, &installed)

	stderr := g.runHeadlessSelfHeal(t)
	if got := readProbe(t, probe); got != staleInstalledSkill {
		t.Fatal("provably older payload overwrote the tree")
	}
	if !strings.Contains(stderr, "payload is older than the installed payload") {
		t.Fatalf("stderr = %q, want the older-payload hold reason", stderr)
	}
}

func TestSelfHealNoDowngradeRefreshesAndStampsLegacyTree(t *testing.T) {
	g := newTestGroup()

	home := g.setupSkillStartupTest(t)
	t.Chdir(t.TempDir())
	g.stubSkillPayloadBuildInfo(t, "abc1234", "v1.0.0-5-gabc1234")
	dir, probe := seedStampedCodexTree(t, home, nil)

	g.runHeadlessSelfHeal(t)
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
	g := newTestGroup()

	home := g.setupSkillStartupTest(t)
	_, second := setupOrderedCheckout(t)
	t.Setenv(trustCheckoutSkillSourcesEnv, "1")
	installed := libskillinstall.PayloadRecord{Origin: libskillinstall.OriginEmbedded, Revision: "fffffff", Version: "v9.9.9", Writer: libskillinstall.WriterExplicit}
	dir := filepath.Join(home, ".codex", "skills")
	if err := libskillinstall.ExtractRecorded(dir, gateSkillFS(), installed); err != nil {
		t.Fatal(err)
	}

	g.runHeadlessSelfHeal(t)
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
	g := newTestGroup()

	home := g.setupSkillStartupTest(t)
	first, second := setupOrderedCheckout(t)
	g.stubSkillPayloadBuildInfo(t, first[:9], "v1.0.0-1-g"+first[:9])
	installed := libskillinstall.PayloadRecord{Origin: libskillinstall.OriginCheckout, Revision: second, Writer: libskillinstall.WriterExplicit}
	_, probe := seedStampedCodexTree(t, home, &installed)
	g.setAvailableSkillAgents(map[string]bool{"codex": true})
	calls := g.setSkillPromptAnswers(t)

	stderr := captureStderr(t, func() {
		if err := g.maybeInstallSkills(); err != nil {
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
	g := newTestGroup()

	home := g.setupSkillStartupTest(t)
	t.Chdir(t.TempDir())
	g.stubSkillPayloadBuildInfo(t, "abc1234", "v1.0.0-5-gabc1234")
	dir, _ := seedStampedCodexTree(t, home, nil)
	g.setAvailableSkillAgents(map[string]bool{"codex": true})
	g.setSkillPromptAnswers(t, "")

	captureStderr(t, func() {
		if err := g.maybeInstallSkills(); err != nil {
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
	g := newTestGroup()

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
			home := g.setupSkillStartupTest(t)
			t.Chdir(t.TempDir())
			g.stubSkillPayloadBuildInfo(t, "abc1234", "dev")
			g.setAvailableSkillAgents(map[string]bool{"codex": true})
			dir := filepath.Join(home, ".codex", "skills")
			switch {
			case tc.fresh:
			case tc.seed != nil:
				seedStampedCodexTree(t, home, tc.seed)
			default:
				if err := libskillinstall.Extract(dir, skillpayload.SkillsFS); err != nil {
					t.Fatal(err)
				}
			}
			var out bytes.Buffer
			if err := g.runSkillSync(&out, tc.mode, "codex"); err != nil {
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

func TestSkillHoldNoticeLifecycle_BossSkills_AC10(t *testing.T) {
	g := newTestGroup()
	home := g.setupSkillStartupTest(t)
	t.Chdir(t.TempDir())
	g.stubSkillPayloadBuildInfo(t, "abc1234", "dev")
	installed := libskillinstall.PayloadRecord{Origin: libskillinstall.OriginCheckout, Revision: "0123456789abcdef0123456789abcdef01234567", Writer: libskillinstall.WriterExplicit}
	dir, probe := seedStampedCodexTree(t, home, &installed)
	for i, want := range []bool{true, false} {
		if stderr := g.runHeadlessSelfHeal(t); strings.Contains(stderr, "held Codex skill refresh") != want {
			t.Fatalf("startup %d stderr = %q, want hold %t", i, stderr, want)
		}
	}
	if readProbe(t, probe) != staleInstalledSkill {
		t.Fatal("hold changed installed bytes")
	}
	if record, _ := mustPayloadRecord(t, dir); record != installed {
		t.Fatalf("hold changed record: %+v", record)
	}
	settings, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	if len(settings.SkillsDeclinedByAgent) != 0 || len(settings.SkillsDeclinedManifestByAgent) != 0 || len(settings.SkillsInstalledManifestByAgent) != 0 {
		t.Fatal("hold recorded decline or installed manifest")
	}
	g.stubSkillPayloadBuildInfo(t, "def5678", "dev")
	if stderr := g.runHeadlessSelfHeal(t); !strings.Contains(stderr, "held Codex skill refresh") {
		t.Fatalf("changed binary not announced: %q", stderr)
	}
	installed.Revision = "1123456789abcdef0123456789abcdef01234567"
	seedStampedCodexTree(t, home, &installed)
	if stderr := g.runHeadlessSelfHeal(t); !strings.Contains(stderr, "held Codex skill refresh") {
		t.Fatalf("changed installed record not announced: %q", stderr)
	}
}

func TestInteractiveSkillHoldNoticeOnce_BossSkills_AC10(t *testing.T) {
	g := newTestGroup()
	home := g.setupSkillStartupTest(t)
	first, second := setupOrderedCheckout(t)
	g.stubSkillPayloadBuildInfo(t, first[:9], "v1.0.0-1-g"+first[:9])
	installed := libskillinstall.PayloadRecord{Origin: libskillinstall.OriginCheckout, Revision: second, Writer: libskillinstall.WriterExplicit}
	seedStampedCodexTree(t, home, &installed)
	g.setAvailableSkillAgents(map[string]bool{"codex": true})
	calls := g.setSkillPromptAnswers(t)
	for _, want := range []bool{true, false} {
		stderr := captureStderr(t, func() {
			if err := g.maybeInstallSkills(); err != nil {
				t.Fatal(err)
			}
		})
		if strings.Contains(stderr, "held Codex skill refresh") != want || *calls != 0 {
			t.Fatalf("hold = %q, prompts = %d, want hold %t and no prompt", stderr, *calls, want)
		}
	}
	if stderr := g.runHeadlessSelfHeal(t); strings.Contains(stderr, "held Codex skill refresh") {
		t.Fatalf("writer mode re-announced the same pair: %q", stderr)
	}
}

func TestSkillHoldNoticeMalformedSettings(t *testing.T) {
	for _, terminal := range []bool{false, true} {
		t.Run(fmt.Sprint(terminal), func(t *testing.T) {
			g := newTestGroup()
			home := g.setupSkillStartupTest(t)
			first, second := setupOrderedCheckout(t)
			g.stubSkillPayloadBuildInfo(t, first[:9], "v1.0.0-1-g"+first[:9])
			installed := libskillinstall.PayloadRecord{Origin: libskillinstall.OriginCheckout, Revision: second, Writer: libskillinstall.WriterExplicit}
			seedStampedCodexTree(t, home, &installed)
			path := os.Getenv("BOSS_SETTINGS_PATH")
			if err := os.WriteFile(path, []byte("{broken"), 0o600); err != nil {
				t.Fatal(err)
			}
			g.setAvailableSkillAgents(map[string]bool{"codex": true})
			g.StartupIsTerminal = func() bool { return terminal }
			g.StartupReadAnswer = func() string { t.Fatal("hold prompted"); return "" }
			for range 2 {
				stderr := captureStderr(t, func() {
					if err := g.maybeInstallSkills(); err != nil {
						t.Fatal(err)
					}
				})
				if !strings.Contains(stderr, "held Codex skill refresh") {
					t.Fatalf("hold missing: %q", stderr)
				}
				if readProbe(t, path) != "{broken" {
					t.Fatal("malformed settings overwritten")
				}
			}
		})
	}
}

func TestSkillCheckReportsHoldAfterStartup_BossSkills_AC11(t *testing.T) {
	g := newTestGroup()
	home := g.setupSkillStartupTest(t)
	t.Chdir(t.TempDir())
	g.stubSkillPayloadBuildInfo(t, "abc1234", "dev")
	installed := libskillinstall.PayloadRecord{Origin: libskillinstall.OriginCheckout, Revision: "0123456789abcdef0123456789abcdef01234567", Writer: libskillinstall.WriterExplicit}
	dir, _ := seedStampedCodexTree(t, home, &installed)
	if err := config.Save(config.Settings{}); err != nil {
		t.Fatal(err)
	}
	g.runHeadlessSelfHeal(t)
	before := readProbe(t, os.Getenv("BOSS_SETTINGS_PATH"))
	for range 2 {
		var out bytes.Buffer
		if err := g.runSkillCheck(&out, "codex"); err == nil {
			t.Fatal("stale tree should still report drift")
		}
		if !strings.Contains(out.String(), "  held: ") {
			t.Fatalf("hold absent: %s", &out)
		}
		if readProbe(t, os.Getenv("BOSS_SETTINGS_PATH")) != before {
			t.Fatal("check changed settings")
		}
	}
	if err := libskillinstall.ExtractRecorded(dir, skillpayload.SkillsFS, installed); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := g.runSkillCheck(&out, "codex"); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out.String(), "  held: ") {
		t.Fatalf("current tree reports hold: %s", &out)
	}
	installed.Writer = libskillinstall.WriterUnattended
	installed.Origin = libskillinstall.OriginEmbedded
	seedStampedCodexTree(t, home, &installed)
	out.Reset()
	_ = g.runSkillCheck(&out, "codex")
	if strings.Contains(out.String(), "  held: ") {
		t.Fatalf("refreshable tree reports hold: %s", &out)
	}
}

func TestSkillInstallClearsHeldNotice(t *testing.T) {
	g := newTestGroup()
	home := g.setupSkillStartupTest(t)
	t.Chdir(t.TempDir())
	g.stubSkillPayloadBuildInfo(t, "abc1234", "dev")
	installed := libskillinstall.PayloadRecord{Origin: libskillinstall.OriginCheckout, Revision: "0123456789abcdef0123456789abcdef01234567", Writer: libskillinstall.WriterExplicit}
	seedStampedCodexTree(t, home, &installed)
	g.runHeadlessSelfHeal(t)
	settings, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	if settings.SkillsHeldNoticeByAgent["codex"] == "" {
		t.Fatal("hold notice not persisted")
	}
	var out bytes.Buffer
	if err := g.runSkillSync(&out, skillSyncInstall, "codex"); err != nil {
		t.Fatal(err)
	}
	settings, err = config.Load()
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := settings.SkillsHeldNoticeByAgent["codex"]; ok {
		t.Fatal("install left stale notice")
	}
	seedStampedCodexTree(t, home, &installed)
	if stderr := g.runHeadlessSelfHeal(t); !strings.Contains(stderr, "held Codex skill refresh") {
		t.Fatalf("new hold not announced: %q", stderr)
	}
}
