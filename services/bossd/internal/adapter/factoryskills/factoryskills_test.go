package factoryskills

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/recurser/bossalib/contract/plugin/skillinstall"
	"github.com/recurser/bossalib/kit/config"
	"github.com/rs/zerolog"
)

func testAgentDirs(t *testing.T) (map[skillinstall.Agent]string, func(skillinstall.Agent) (string, error)) {
	t.Helper()
	root := t.TempDir()
	dirs := make(map[skillinstall.Agent]string, len(skillinstall.AgentCLIs()))
	for _, target := range skillinstall.AgentCLIs() {
		dirs[target.Agent] = filepath.Join(root, string(target.Agent))
	}
	return dirs, func(agent skillinstall.Agent) (string, error) {
		dir, ok := dirs[agent]
		if !ok {
			return "", errors.New("unknown agent")
		}
		return dir, nil
	}
}

func testLookPath(available ...string) func(string) (string, error) {
	have := make(map[string]bool, len(available))
	for _, cmd := range available {
		have[cmd] = true
	}
	return func(cmd string) (string, error) {
		if have[cmd] {
			return "/usr/local/bin/" + cmd, nil
		}
		return "", errors.New("not found")
	}
}

func nonEmptyLines(raw string) []string {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return nil
	}
	return strings.Split(trimmed, "\n")
}

func TestInstallerInstallOnConsent(t *testing.T) {
	payload := fstest.MapFS{
		"skills/boss-build/SKILL.md": {Data: []byte("embedded v1.2.0")},
	}
	manifest, err := skillinstall.Manifest(payload)
	if err != nil {
		t.Fatalf("Manifest: %v", err)
	}

	mustMarshal := func(t *testing.T, s config.Settings) []byte {
		t.Helper()
		b, err := json.Marshal(s)
		if err != nil {
			t.Fatalf("marshal settings: %v", err)
		}
		return b
	}

	tests := []struct {
		name              string
		reason            string
		settingsBytes     []byte
		skipEnv           string
		availableCLIs     []string
		preInstallAll     bool
		wantClaudeInstall bool
		wantLogCount      int
		wantLogLevel      string
		wantLogSubstrings []string
	}{
		{
			name:              "fresh install with missing settings logs one line",
			reason:            "repo-register",
			availableCLIs:     []string{"claude"},
			wantClaudeInstall: true,
			wantLogCount:      1,
			wantLogLevel:      "info",
			wantLogSubstrings: []string{
				"boss skills: consent install (repo-register)",
				"claude=install",
				"codex=no-cli",
			},
		},
		{
			name:   "legacy SkillsDeclined bit still installs",
			reason: "repo-register",
			settingsBytes: mustMarshal(t, config.Settings{
				SkillsDeclined: true,
			}),
			availableCLIs:     []string{"claude"},
			wantClaudeInstall: true,
			wantLogCount:      1,
			wantLogLevel:      "info",
			wantLogSubstrings: []string{
				"boss skills: consent install (repo-register)",
				"claude=install",
			},
		},
		{
			name:              "BOSS_SKIP_SKILLS skips install",
			reason:            "repo-register",
			skipEnv:           "1",
			availableCLIs:     []string{"claude"},
			wantClaudeInstall: false,
			wantLogCount:      1,
			wantLogLevel:      "info",
			wantLogSubstrings: []string{
				"boss skills: consent install (repo-register)",
				"claude=skipped-env",
			},
		},
		{
			name:   "same-manifest per-agent decline skips install",
			reason: "repo-register",
			settingsBytes: mustMarshal(t, config.Settings{
				SkillsDeclinedByAgent:         map[string]bool{"claude": true},
				SkillsDeclinedManifestByAgent: map[string]string{"claude": manifest},
			}),
			availableCLIs:     []string{"claude"},
			wantClaudeInstall: false,
			wantLogCount:      1,
			wantLogLevel:      "info",
			wantLogSubstrings: []string{
				"boss skills: consent install (repo-register)",
				"claude=declined",
			},
		},
		{
			name:   "other-manifest per-agent decline installs",
			reason: "repo-register",
			settingsBytes: mustMarshal(t, config.Settings{
				SkillsDeclinedByAgent:         map[string]bool{"claude": true},
				SkillsDeclinedManifestByAgent: map[string]string{"claude": "different-manifest"},
			}),
			availableCLIs:     []string{"claude"},
			wantClaudeInstall: true,
			wantLogCount:      1,
			wantLogLevel:      "info",
			wantLogSubstrings: []string{
				"boss skills: consent install (repo-register)",
				"claude=install",
			},
		},
		{
			name:              "malformed settings file fails closed and warns",
			reason:            "repo-register",
			settingsBytes:     []byte("{not valid json"),
			availableCLIs:     []string{"claude"},
			wantClaudeInstall: false,
			wantLogCount:      1,
			wantLogLevel:      "warn",
			wantLogSubstrings: []string{
				"boss skills: consent install (repo-register)",
				"claude=settings-unreadable",
			},
		},
		{
			name:              "all agents already present logs nothing",
			reason:            "cron-create",
			availableCLIs:     []string{"claude", "codex", "hermes", "agy"},
			preInstallAll:     true,
			wantClaudeInstall: true,
			wantLogCount:      0,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			dirs, dirFor := testAgentDirs(t)
			settingsPath := filepath.Join(t.TempDir(), "settings.json")
			if tc.settingsBytes != nil {
				if err := os.WriteFile(settingsPath, tc.settingsBytes, 0o600); err != nil {
					t.Fatalf("write settings: %v", err)
				}
			}
			if tc.preInstallAll {
				rec := skillinstall.EmbeddedPayloadRecord("1111111", "v1.2.0", skillinstall.WriterExplicit)
				for _, target := range skillinstall.AgentCLIs() {
					if err := skillinstall.ExtractRecorded(dirs[target.Agent], payload, rec); err != nil {
						t.Fatalf("preinstall %s: %v", target.Agent, err)
					}
				}
			}

			var logBuf bytes.Buffer
			inst := New(zerolog.New(&logBuf))
			inst.FS = payload
			inst.DirFor = dirFor
			inst.LookPath = testLookPath(tc.availableCLIs...)
			inst.LoadSettings = func() (config.Settings, error) { return config.LoadFrom(settingsPath) }
			inst.Getenv = func(k string) string {
				if k == skipSkillsEnv {
					return tc.skipEnv
				}
				return ""
			}
			inst.BuildInfo = func() (string, string) { return "abcdef1", "v1.2.0" }

			inst.InstallOnConsent(context.Background(), tc.reason)

			gotInstalled := skillinstall.IsInstalled(dirs[skillinstall.AgentClaude])
			if gotInstalled != tc.wantClaudeInstall {
				t.Fatalf("claude installed = %v, want %v", gotInstalled, tc.wantClaudeInstall)
			}
			if tc.wantClaudeInstall && !tc.preInstallAll {
				rec, recorded, err := skillinstall.ReadPayloadRecord(dirs[skillinstall.AgentClaude])
				if err != nil || !recorded {
					t.Fatalf("ReadPayloadRecord = (%+v, %v, %v), want recorded", rec, recorded, err)
				}
				if rec.Writer != skillinstall.WriterUnattended || rec.Origin != skillinstall.OriginEmbedded {
					t.Fatalf("record = %+v, want embedded unattended", rec)
				}
			}

			// Verify settings.json was never created or modified.
			if tc.settingsBytes == nil {
				if _, err := os.Stat(settingsPath); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("settings.json should not be created, stat err = %v", err)
				}
			} else {
				after, err := os.ReadFile(settingsPath)
				if err != nil {
					t.Fatalf("read settings after install: %v", err)
				}
				if !bytes.Equal(after, tc.settingsBytes) {
					t.Fatalf("settings.json modified: got %q, want %q", after, tc.settingsBytes)
				}
			}

			lines := nonEmptyLines(logBuf.String())
			if len(lines) != tc.wantLogCount {
				t.Fatalf("log line count = %d, want %d; output:\n%s", len(lines), tc.wantLogCount, logBuf.String())
			}
			if tc.wantLogCount == 1 {
				t.Logf("consent log: %s", lines[0])
				if tc.wantLogLevel != "" {
					var entry map[string]any
					if err := json.Unmarshal([]byte(lines[0]), &entry); err != nil {
						t.Fatalf("unmarshal log line: %v", err)
					}
					if entry["level"] != tc.wantLogLevel {
						t.Fatalf("log level = %v, want %q; line=%s", entry["level"], tc.wantLogLevel, lines[0])
					}
				}
				for _, sub := range tc.wantLogSubstrings {
					if !strings.Contains(lines[0], sub) {
						t.Fatalf("log line %q missing substring %q", lines[0], sub)
					}
				}
			}
		})
	}
}

// TestInstallerConsentProof_BossSkills_AC271201_AC271202 links AC271201 and
// AC271202 to the consent-install table while keeping TestInstallerInstallOnConsent's
// exact name.
func TestInstallerConsentProof_BossSkills_AC271201_AC271202(t *testing.T) {
	TestInstallerInstallOnConsent(t)
}

func TestInstallerRefreshAfterUpgrade_BossSkills_AC271203(t *testing.T) {
	oldPayload := fstest.MapFS{
		"skills/boss-build/SKILL.md": {Data: []byte("embedded v1.0.0")},
	}
	newPayload := fstest.MapFS{
		"skills/boss-build/SKILL.md": {Data: []byte("embedded v1.2.0")},
	}

	dirs, dirFor := testAgentDirs(t)
	settingsPath := filepath.Join(t.TempDir(), "settings.json")

	// Claude: installed by older release with older bytes -> should update once.
	olderRec := skillinstall.EmbeddedPayloadRecord("1111111", "v1.0.0", skillinstall.WriterUnattended)
	if err := skillinstall.ExtractRecorded(dirs[skillinstall.AgentClaude], oldPayload, olderRec); err != nil {
		t.Fatalf("seed claude: %v", err)
	}
	// Codex: already matches newPayload bytes but carries older unattended record -> should restamp once.
	if err := skillinstall.ExtractRecorded(dirs[skillinstall.AgentCodex], newPayload, olderRec); err != nil {
		t.Fatalf("seed codex: %v", err)
	}
	// Hermes: installed by a newer release -> should hold with remedy warning.
	newerRec := skillinstall.EmbeddedPayloadRecord("9999999", "v2.0.0", skillinstall.WriterExplicit)
	if err := skillinstall.ExtractRecorded(dirs[skillinstall.AgentHermes], oldPayload, newerRec); err != nil {
		t.Fatalf("seed hermes: %v", err)
	}
	// Antigravity: absent -> must remain absent.

	var logBuf bytes.Buffer
	inst := New(zerolog.New(&logBuf))
	inst.FS = newPayload
	inst.DirFor = dirFor
	inst.LoadSettings = func() (config.Settings, error) { return config.LoadFrom(settingsPath) }
	inst.Getenv = func(string) string { return "" }
	inst.BuildInfo = func() (string, string) { return "abcdef1", "v1.2.0" }

	inst.RefreshAfterUpgrade(context.Background())

	// Claude updated to new payload and stamped with current build.
	claudeSkill := filepath.Join(dirs[skillinstall.AgentClaude], skillinstall.Namespace, "boss-build", "SKILL.md")
	gotClaude, err := os.ReadFile(claudeSkill)
	if err != nil {
		t.Fatalf("read claude skill: %v", err)
	}
	if string(gotClaude) != "embedded v1.2.0" {
		t.Fatalf("claude skill = %q, want updated payload", gotClaude)
	}
	codexRec, recorded, err := skillinstall.ReadPayloadRecord(dirs[skillinstall.AgentCodex])
	if err != nil || !recorded || codexRec.Version != "v1.2.0" {
		t.Fatalf("codex record = (%+v, %v, %v), want restamped v1.2.0", codexRec, recorded, err)
	}
	if skillinstall.IsInstalled(dirs[skillinstall.AgentAntigravity]) {
		t.Fatal("absent antigravity tree must stay absent after refresh")
	}

	firstLogs := nonEmptyLines(logBuf.String())
	if len(firstLogs) != 2 {
		t.Fatalf("first refresh log lines = %d, want 2 (1 hold warn + 1 summary info); output:\n%s", len(firstLogs), logBuf.String())
	}
	var sawHoldWarn, sawSummaryInfo bool
	for _, line := range firstLogs {
		if strings.Contains(line, `"level":"warn"`) &&
			strings.Contains(line, `"agent":"hermes"`) &&
			strings.Contains(line, "run `boss skills install` to replace it explicitly") {
			sawHoldWarn = true
		}
		if strings.Contains(line, `"level":"info"`) &&
			strings.Contains(line, "skill-refresh:") &&
			strings.Contains(line, "updated=claude") &&
			strings.Contains(line, "restamped=codex") {
			sawSummaryInfo = true
		}
	}
	if !sawHoldWarn {
		t.Fatalf("missing held warn line for hermes; output:\n%s", logBuf.String())
	}
	if !sawSummaryInfo {
		t.Fatalf("missing info summary naming updated=claude and restamped=codex; output:\n%s", logBuf.String())
	}

	// Hand-edit Codex tree after the first refresh; a second run on the same build
	// must skip Claude and Codex (preserving the edit) and emit no info line.
	codexSkill := filepath.Join(dirs[skillinstall.AgentCodex], skillinstall.Namespace, "boss-build", "SKILL.md")
	if err := os.WriteFile(codexSkill, []byte("hand edit"), 0o600); err != nil {
		t.Fatalf("hand edit codex: %v", err)
	}
	logBuf.Reset()
	inst.RefreshAfterUpgrade(context.Background())

	gotCodex, err := os.ReadFile(codexSkill)
	if err != nil {
		t.Fatalf("read codex skill: %v", err)
	}
	if string(gotCodex) != "hand edit" {
		t.Fatalf("codex hand edit was overwritten: got %q", gotCodex)
	}
	secondLogs := nonEmptyLines(logBuf.String())
	if len(secondLogs) != 1 || !strings.Contains(secondLogs[0], `"agent":"hermes"`) {
		t.Fatalf("second refresh logs = %v, want only hermes hold warning", secondLogs)
	}

	// BOSS_SKIP_SKILLS skips refresh completely and logs nothing.
	logBuf.Reset()
	inst.Getenv = func(k string) string {
		if k == skipSkillsEnv {
			return "1"
		}
		return ""
	}
	inst.RefreshAfterUpgrade(context.Background())
	if logBuf.Len() != 0 {
		t.Fatalf("expected no logs with BOSS_SKIP_SKILLS, got:\n%s", logBuf.String())
	}
}
