package skillinstall

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"testing/fstest"
)

func TestDecideConsentInstall(t *testing.T) {
	tests := []struct {
		name  string
		facts ConsentFacts
		want  ConsentAction
	}{
		{
			name: "env skip beats everything",
			facts: ConsentFacts{
				SkipEnv:          true,
				Installed:        true,
				CLIFound:         false,
				SettingsReadable: false,
				Declined:         true,
			},
			want: ConsentSkippedEnv,
		},
		{
			name: "present beats missing CLI and unreadable settings",
			facts: ConsentFacts{
				Installed:        true,
				CLIFound:         false,
				SettingsReadable: false,
				Declined:         true,
			},
			want: ConsentPresent,
		},
		{
			name: "missing CLI beats unreadable settings and decline",
			facts: ConsentFacts{
				Installed:        false,
				CLIFound:         false,
				SettingsReadable: false,
				Declined:         true,
			},
			want: ConsentNoCLI,
		},
		{
			name: "unreadable settings fails closed before decline",
			facts: ConsentFacts{
				Installed:        false,
				CLIFound:         true,
				SettingsReadable: false,
				Declined:         false,
			},
			want: ConsentSettingsUnreadable,
		},
		{
			name: "same-manifest decline blocks install",
			facts: ConsentFacts{
				Installed:        false,
				CLIFound:         true,
				SettingsReadable: true,
				Declined:         true,
			},
			want: ConsentDeclined,
		},
		{
			name: "absent with CLI readable settings and not declined installs",
			facts: ConsentFacts{
				Installed:        false,
				CLIFound:         true,
				SettingsReadable: true,
				Declined:         false,
			},
			want: ConsentInstall,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := DecideConsentInstall(tc.facts); got != tc.want {
				t.Fatalf("DecideConsentInstall(%+v) = %q, want %q", tc.facts, got, tc.want)
			}
		})
	}

	t.Run("declines predicate honours same manifest only", func(t *testing.T) {
		declines := Declines{
			ByAgent:         map[string]bool{"claude": true, "codex": false},
			ManifestByAgent: map[string]string{"claude": "manifest-a", "codex": "manifest-a"},
		}
		if !declines.Declined(AgentClaude, "manifest-a") {
			t.Fatal("expected claude to be declined for manifest-a")
		}
		if declines.Declined(AgentClaude, "manifest-b") {
			t.Fatal("other-manifest decline must not block install")
		}
		if declines.Declined(AgentCodex, "manifest-a") {
			t.Fatal("false ByAgent entry must not decline")
		}
		if declines.Declined(AgentHermes, "manifest-a") {
			t.Fatal("absent agent must not decline")
		}
	})
}

func makeAgentDirs(root string) (map[Agent]string, func(Agent) (string, error)) {
	dirs := map[Agent]string{}
	for _, a := range AgentCLIs() {
		dirs[a.Agent] = filepath.Join(root, string(a.Agent))
	}
	return dirs, func(agent Agent) (string, error) {
		dir, ok := dirs[agent]
		if !ok {
			return "", errors.New("unknown agent")
		}
		return dir, nil
	}
}

func fakeLookPath(available ...string) func(string) (string, error) {
	set := make(map[string]bool, len(available))
	for _, cmd := range available {
		set[cmd] = true
	}
	return func(cmd string) (string, error) {
		if set[cmd] {
			return "/usr/local/bin/" + cmd, nil
		}
		return "", errors.New("not found")
	}
}

func outcomesByAgent(outcomes []ConsentOutcome) map[Agent]ConsentOutcome {
	m := make(map[Agent]ConsentOutcome, len(outcomes))
	for _, o := range outcomes {
		m[o.Agent] = o
	}
	return m
}

func refreshByAgent(outcomes []RefreshOutcome) map[Agent]RefreshOutcome {
	m := make(map[Agent]RefreshOutcome, len(outcomes))
	for _, o := range outcomes {
		m[o.Agent] = o
	}
	return m
}

func TestInstallOnConsent(t *testing.T) {
	payload := fstest.MapFS{
		"skills/boss-build/SKILL.md": {Data: []byte("embedded v1.2.0")},
	}
	manifest, err := Manifest(payload)
	if err != nil {
		t.Fatal(err)
	}
	record := EmbeddedPayloadRecord("1234567", "v1.2.0", WriterUnattended)

	t.Run("installs absent agents with CLI, skips missing CLI, leaves existing untouched", func(t *testing.T) {
		dirs, dirFor := makeAgentDirs(t.TempDir())

		// Pre-populate Codex with custom bytes and an explicit record.
		existingPayload := fstest.MapFS{
			"skills/boss-build/SKILL.md": {Data: []byte("custom existing codex")},
		}
		existingRecord := EmbeddedPayloadRecord("0000001", "v1.0.0", WriterExplicit)
		if err := ExtractRecorded(dirs[AgentCodex], existingPayload, existingRecord); err != nil {
			t.Fatal(err)
		}

		outcomes := outcomesByAgent(InstallOnConsent(ConsentEnv{
			FS:               payload,
			Record:           record,
			Manifest:         manifest,
			SettingsReadable: true,
			LookPath:         fakeLookPath("claude", "codex", "agy"),
			DirFor:           dirFor,
		}))

		// Claude and Antigravity should be freshly installed and stamped unattended.
		for _, agent := range []Agent{AgentClaude, AgentAntigravity} {
			got := outcomes[agent]
			if got.Action != ConsentInstall || !got.Installed || got.Err != nil {
				t.Fatalf("%s outcome = %+v, want install", agent, got)
			}
			data, err := os.ReadFile(filepath.Join(dirs[agent], Namespace, "boss-build", "SKILL.md"))
			if err != nil || string(data) != "embedded v1.2.0" {
				t.Fatalf("%s content = %q (%v)", agent, data, err)
			}
			rec, recorded, err := ReadPayloadRecord(dirs[agent])
			if err != nil || !recorded || rec != record {
				t.Fatalf("%s record = %+v (recorded=%v, err=%v), want %+v", agent, rec, recorded, err, record)
			}
		}

		// Codex was already present and must remain byte-for-byte intact.
		if got := outcomes[AgentCodex]; got.Action != ConsentPresent || got.Installed || got.Err != nil {
			t.Fatalf("codex outcome = %+v, want present", got)
		}
		codexBytes, err := os.ReadFile(filepath.Join(dirs[AgentCodex], Namespace, "boss-build", "SKILL.md"))
		if err != nil || string(codexBytes) != "custom existing codex" {
			t.Fatalf("codex bytes modified: %q (%v)", codexBytes, err)
		}
		codexRec, recorded, err := ReadPayloadRecord(dirs[AgentCodex])
		if err != nil || !recorded || codexRec != existingRecord {
			t.Fatalf("codex record modified: %+v (%v, %v)", codexRec, recorded, err)
		}

		// Hermes had no CLI on PATH and must remain absent.
		if got := outcomes[AgentHermes]; got.Action != ConsentNoCLI || got.Installed || got.Err != nil {
			t.Fatalf("hermes outcome = %+v, want no-cli", got)
		}
		if IsInstalled(dirs[AgentHermes]) {
			t.Fatal("hermes must not be installed without CLI")
		}
	})

	t.Run("honours SkipEnv, unreadable settings, same-manifest and other-manifest declines", func(t *testing.T) {
		dirs, dirFor := makeAgentDirs(t.TempDir())

		// SkipEnv skips every agent.
		skipped := outcomesByAgent(InstallOnConsent(ConsentEnv{
			FS:               payload,
			Record:           record,
			Manifest:         manifest,
			SettingsReadable: true,
			SkipEnv:          true,
			LookPath:         fakeLookPath("claude", "codex", "hermes", "agy"),
			DirFor:           dirFor,
		}))
		for _, a := range AgentCLIs() {
			if got := skipped[a.Agent]; got.Action != ConsentSkippedEnv || got.Installed {
				t.Fatalf("skip env %s = %+v", a.Agent, got)
			}
			if IsInstalled(dirs[a.Agent]) {
				t.Fatalf("skip env installed %s", a.Agent)
			}
		}

		// Unreadable settings fails closed for all absent agents with CLI.
		unreadable := outcomesByAgent(InstallOnConsent(ConsentEnv{
			FS:               payload,
			Record:           record,
			Manifest:         manifest,
			SettingsReadable: false,
			LookPath:         fakeLookPath("claude"),
			DirFor:           dirFor,
		}))
		if got := unreadable[AgentClaude]; got.Action != ConsentSettingsUnreadable || got.Installed {
			t.Fatalf("unreadable claude = %+v", got)
		}
		if IsInstalled(dirs[AgentClaude]) {
			t.Fatal("unreadable settings must not install claude")
		}

		// Same-manifest decline skips claude; other-manifest decline installs codex.
		declined := outcomesByAgent(InstallOnConsent(ConsentEnv{
			FS:               payload,
			Record:           record,
			Manifest:         manifest,
			SettingsReadable: true,
			Declines: Declines{
				ByAgent:         map[string]bool{"claude": true, "codex": true},
				ManifestByAgent: map[string]string{"claude": manifest, "codex": "older-manifest"},
			},
			LookPath: fakeLookPath("claude", "codex"),
			DirFor:   dirFor,
		}))
		if got := declined[AgentClaude]; got.Action != ConsentDeclined || got.Installed {
			t.Fatalf("same-manifest claude = %+v", got)
		}
		if IsInstalled(dirs[AgentClaude]) {
			t.Fatal("same-manifest declined claude must stay absent")
		}
		if got := declined[AgentCodex]; got.Action != ConsentInstall || !got.Installed || got.Err != nil {
			t.Fatalf("other-manifest codex = %+v", got)
		}
		if !IsInstalled(dirs[AgentCodex]) {
			t.Fatal("other-manifest declined codex should install")
		}
	})
}

func TestInstallIfAbsent(t *testing.T) {
	v1 := fstest.MapFS{"skills/boss-build/SKILL.md": {Data: []byte("v1")}}
	v2 := fstest.MapFS{"skills/boss-build/SKILL.md": {Data: []byte("v2")}}
	recV1 := EmbeddedPayloadRecord("1111111", "v1.0.0", WriterExplicit)
	recV2 := EmbeddedPayloadRecord("2222222", "v2.0.0", WriterUnattended)

	t.Run("installs when absent and skips when already present", func(t *testing.T) {
		dir := t.TempDir()
		installed, err := InstallIfAbsent(dir, v1, recV1)
		if err != nil || !installed {
			t.Fatalf("first InstallIfAbsent = (%v, %v), want (true, nil)", installed, err)
		}
		installed, err = InstallIfAbsent(dir, v2, recV2)
		if err != nil || installed {
			t.Fatalf("second InstallIfAbsent = (%v, %v), want (false, nil)", installed, err)
		}
		data, err := os.ReadFile(filepath.Join(dir, Namespace, "boss-build", "SKILL.md"))
		if err != nil || string(data) != "v1" {
			t.Fatalf("existing tree overwritten: %q (%v)", data, err)
		}
		gotRec, recorded, err := ReadPayloadRecord(dir)
		if err != nil || !recorded || gotRec != recV1 {
			t.Fatalf("existing record overwritten: %+v (%v, %v)", gotRec, recorded, err)
		}
	})

	t.Run("never overwrites tree installed before update lock is acquired", func(t *testing.T) {
		dir := t.TempDir()
		lock, err := acquireUpdateLock(dir)
		if err != nil {
			t.Fatal(err)
		}
		unlocked := false
		defer func() {
			if !unlocked {
				_ = lock.Unlock()
			}
		}()

		type result struct {
			installed bool
			err       error
		}
		started := make(chan struct{})
		done := make(chan result, 1)
		go func() {
			close(started)
			ok, err := InstallIfAbsent(dir, v2, recV2)
			done <- result{installed: ok, err: err}
		}()

		<-started
		// Simulate an explicit `boss skills install` completing while holding the lock.
		if err := extract(dir, v1, &recV1); err != nil {
			t.Fatal(err)
		}
		if err := lock.Unlock(); err != nil {
			t.Fatal(err)
		}
		unlocked = true

		got := <-done
		if got.err != nil || got.installed {
			t.Fatalf("raced InstallIfAbsent = %+v, want (false, nil)", got)
		}
		data, err := os.ReadFile(filepath.Join(dir, Namespace, "boss-build", "SKILL.md"))
		if err != nil || string(data) != "v1" {
			t.Fatalf("concurrent explicit install overwritten: %q (%v)", data, err)
		}
		gotRec, recorded, err := ReadPayloadRecord(dir)
		if err != nil || !recorded || gotRec != recV1 {
			t.Fatalf("concurrent explicit record overwritten: %+v (%v, %v)", gotRec, recorded, err)
		}
	})
}

func TestRefreshInstalled(t *testing.T) {
	v1FS := fstest.MapFS{"skills/boss-build/SKILL.md": {Data: []byte("v1.0.0 body")}}
	v2FS := fstest.MapFS{"skills/boss-build/SKILL.md": {Data: []byte("v2.0.0 body")}}
	v3FS := fstest.MapFS{"skills/boss-build/SKILL.md": {Data: []byte("v3.0.0 body")}}

	v2Manifest, err := Manifest(v2FS)
	if err != nil {
		t.Fatal(err)
	}

	recV1Unattended := EmbeddedPayloadRecord("1111111", "v1.0.0", WriterUnattended)
	recV2Unattended := EmbeddedPayloadRecord("2222222", "v2.0.0", WriterUnattended)
	recV3Unattended := EmbeddedPayloadRecord("3333333", "v3.0.0", WriterUnattended)

	t.Run("updates older release once and preserves local edit on second run", func(t *testing.T) {
		dirs, dirFor := makeAgentDirs(t.TempDir())
		if err := ExtractRecorded(dirs[AgentCodex], v1FS, recV1Unattended); err != nil {
			t.Fatal(err)
		}

		// CLI lookup is intentionally failing: refresh does not require CLI on PATH.
		env := ConsentEnv{
			FS:               v2FS,
			Record:           recV2Unattended,
			Manifest:         v2Manifest,
			SettingsReadable: true,
			LookPath:         fakeLookPath(),
			DirFor:           dirFor,
		}

		first := refreshByAgent(RefreshInstalled(env))
		if got := first[AgentCodex]; !got.Updated || got.Restamped || got.Held || got.Err != nil {
			t.Fatalf("first refresh codex = %+v, want Updated", got)
		}
		if IsInstalled(dirs[AgentClaude]) {
			t.Fatal("absent claude tree must stay absent")
		}

		// Local edit made after the upgrade must survive a second run of the same build.
		skillFile := filepath.Join(dirs[AgentCodex], Namespace, "boss-build", "SKILL.md")
		if err := os.WriteFile(skillFile, []byte("local hand edit"), 0o644); err != nil {
			t.Fatal(err)
		}
		second := refreshByAgent(RefreshInstalled(env))
		if got := second[AgentCodex]; got.Updated || got.Restamped || got.Held || got.Err != nil {
			t.Fatalf("second refresh codex = %+v, want skip", got)
		}
		data, err := os.ReadFile(skillFile)
		if err != nil || string(data) != "local hand edit" {
			t.Fatalf("local edit overwritten on second run: %q (%v)", data, err)
		}
	})

	t.Run("restamps matching tree with older unattended record once then skips", func(t *testing.T) {
		dirs, dirFor := makeAgentDirs(t.TempDir())
		// Same bytes as v2FS, but stamped by older v1.0.0 unattended release.
		if err := ExtractRecorded(dirs[AgentHermes], v2FS, recV1Unattended); err != nil {
			t.Fatal(err)
		}
		env := ConsentEnv{
			FS:               v2FS,
			Record:           recV2Unattended,
			Manifest:         v2Manifest,
			SettingsReadable: true,
			DirFor:           dirFor,
		}
		first := refreshByAgent(RefreshInstalled(env))
		if got := first[AgentHermes]; got.Updated || !got.Restamped || got.Held || got.Err != nil {
			t.Fatalf("first restamp hermes = %+v, want Restamped", got)
		}
		rec, recorded, err := ReadPayloadRecord(dirs[AgentHermes])
		if err != nil || !recorded || rec != recV2Unattended {
			t.Fatalf("restamped record = %+v (%v, %v), want %+v", rec, recorded, err, recV2Unattended)
		}

		// Local edit now survives because the restamp tripped the same-build gate.
		skillFile := filepath.Join(dirs[AgentHermes], Namespace, "boss-build", "SKILL.md")
		if err := os.WriteFile(skillFile, []byte("hermes edit"), 0o644); err != nil {
			t.Fatal(err)
		}
		second := refreshByAgent(RefreshInstalled(env))
		if got := second[AgentHermes]; got.Updated || got.Restamped || got.Held || got.Err != nil {
			t.Fatalf("second run after restamp = %+v, want skip", got)
		}
		data, err := os.ReadFile(skillFile)
		if err != nil || string(data) != "hermes edit" {
			t.Fatalf("local edit overwritten after restamp: %q (%v)", data, err)
		}
	})

	t.Run("never restamps vouched explicit checkout or pinned records", func(t *testing.T) {
		dirs, dirFor := makeAgentDirs(t.TempDir())
		explicitRec := EmbeddedPayloadRecord("1111111", "v1.0.0", WriterExplicit)
		checkoutRec := PayloadRecord{
			Origin:   OriginCheckout,
			Revision: "1111111",
			Version:  "v1.0.0",
			Writer:   WriterUnattended,
		}
		pinnedRec := EmbeddedPayloadRecord("1111111", "v1.0.0", WriterUnattended)
		pinnedRec.Pinned = true

		if err := ExtractRecorded(dirs[AgentClaude], v2FS, explicitRec); err != nil {
			t.Fatal(err)
		}
		if err := ExtractRecorded(dirs[AgentCodex], v2FS, checkoutRec); err != nil {
			t.Fatal(err)
		}
		if err := ExtractRecorded(dirs[AgentHermes], v2FS, pinnedRec); err != nil {
			t.Fatal(err)
		}

		outcomes := refreshByAgent(RefreshInstalled(ConsentEnv{
			FS:               v2FS,
			Record:           recV2Unattended,
			Manifest:         v2Manifest,
			SettingsReadable: true,
			DirFor:           dirFor,
		}))

		cases := []struct {
			agent Agent
			want  PayloadRecord
		}{
			{AgentClaude, explicitRec},
			{AgentCodex, checkoutRec},
			{AgentHermes, pinnedRec},
		}
		for _, tc := range cases {
			if got := outcomes[tc.agent]; got.Updated || got.Restamped || got.Held || got.Err != nil {
				t.Fatalf("vouched %s outcome = %+v, want no-op", tc.agent, got)
			}
			rec, recorded, err := ReadPayloadRecord(dirs[tc.agent])
			if err != nil || !recorded || rec != tc.want {
				t.Fatalf("vouched %s record changed to %+v, want %+v", tc.agent, rec, tc.want)
			}
		}
	})

	t.Run("holds over newer release and honours SkipEnv declines and unreadable settings", func(t *testing.T) {
		dirs, dirFor := makeAgentDirs(t.TempDir())
		if err := ExtractRecorded(dirs[AgentClaude], v3FS, recV3Unattended); err != nil {
			t.Fatal(err)
		}
		if err := ExtractRecorded(dirs[AgentCodex], v1FS, recV1Unattended); err != nil {
			t.Fatal(err)
		}
		if err := ExtractRecorded(dirs[AgentHermes], v1FS, recV1Unattended); err != nil {
			t.Fatal(err)
		}

		// SkipEnv skips everything.
		skipped := refreshByAgent(RefreshInstalled(ConsentEnv{
			FS:               v2FS,
			Record:           recV2Unattended,
			Manifest:         v2Manifest,
			SettingsReadable: true,
			SkipEnv:          true,
			DirFor:           dirFor,
		}))
		for _, a := range AgentCLIs() {
			if got := skipped[a.Agent]; got.Updated || got.Restamped || got.Held || got.Err != nil {
				t.Fatalf("SkipEnv %s = %+v", a.Agent, got)
			}
		}

		// Readable settings with codex declined: claude holds (newer v3), codex skipped (declined), hermes updates.
		outcomes := refreshByAgent(RefreshInstalled(ConsentEnv{
			FS:               v2FS,
			Record:           recV2Unattended,
			Manifest:         v2Manifest,
			SettingsReadable: true,
			Declines: Declines{
				ByAgent:         map[string]bool{"codex": true},
				ManifestByAgent: map[string]string{"codex": v2Manifest},
			},
			DirFor: dirFor,
		}))
		if got := outcomes[AgentClaude]; !got.Held || got.Updated || got.Decision.Ordering != OrderOlder || got.Err != nil {
			t.Fatalf("claude newer release = %+v, want Held/OrderOlder", got)
		}
		if got := outcomes[AgentCodex]; got.Updated || got.Restamped || got.Held || got.Err != nil {
			t.Fatalf("declined codex = %+v, want skip", got)
		}
		if got := outcomes[AgentHermes]; !got.Updated || got.Held || got.Err != nil {
			t.Fatalf("hermes older release = %+v, want Updated", got)
		}

		// Unreadable settings proceeds with refresh (matches selfHealSkills).
		unreadable := refreshByAgent(RefreshInstalled(ConsentEnv{
			FS:               v2FS,
			Record:           recV2Unattended,
			Manifest:         v2Manifest,
			SettingsReadable: false,
			Declines: Declines{
				ByAgent:         map[string]bool{"codex": true},
				ManifestByAgent: map[string]string{"codex": v2Manifest},
			},
			DirFor: dirFor,
		}))
		if got := unreadable[AgentCodex]; !got.Updated || got.Err != nil {
			t.Fatalf("unreadable settings codex = %+v, want Updated", got)
		}
	})

	t.Run("preserves concurrent vouched or newer record written after unlocked read", func(t *testing.T) {
		explicitRec := EmbeddedPayloadRecord("2222222", "v1.5.0", WriterExplicit)
		checkoutRec := PayloadRecord{
			Origin:   OriginCheckout,
			Revision: "2222222",
			Version:  "v1.5.0",
			Writer:   WriterUnattended,
		}
		pinnedRec := EmbeddedPayloadRecord("2222222", "v1.5.0", WriterUnattended)
		pinnedRec.Pinned = true

		cases := []struct {
			name       string
			legacyInit bool
			initial    PayloadRecord
			concurrent PayloadRecord
		}{
			{
				name:       "unattended replaced by concurrent explicit install",
				initial:    recV1Unattended,
				concurrent: explicitRec,
			},
			{
				name:       "legacy unstamped replaced by concurrent explicit install",
				legacyInit: true,
				concurrent: explicitRec,
			},
			{
				name:       "unattended replaced by concurrent checkout install",
				initial:    recV1Unattended,
				concurrent: checkoutRec,
			},
			{
				name:       "unattended replaced by concurrent pinned refresh",
				initial:    recV1Unattended,
				concurrent: pinnedRec,
			},
			{
				name:       "unattended replaced by concurrent newer release",
				initial:    recV1Unattended,
				concurrent: recV3Unattended,
			},
		}

		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				dirs, dirFor := makeAgentDirs(t.TempDir())
				dir := dirs[AgentClaude]
				if tc.legacyInit {
					if err := Extract(dir, v2FS); err != nil {
						t.Fatal(err)
					}
				} else {
					if err := ExtractRecorded(dir, v2FS, tc.initial); err != nil {
						t.Fatal(err)
					}
				}

				t.Cleanup(func() { beforeRefreshLock = nil })
				beforeRefreshLock = func() {
					if err := ExtractRecorded(dir, v2FS, tc.concurrent); err != nil {
						t.Errorf("concurrent ExtractRecorded: %v", err)
					}
				}

				outcomes := refreshByAgent(RefreshInstalled(ConsentEnv{
					FS:               v2FS,
					Record:           recV2Unattended,
					Manifest:         v2Manifest,
					SettingsReadable: true,
					DirFor:           dirFor,
				}))
				beforeRefreshLock = nil

				if got := outcomes[AgentClaude]; got.Updated || got.Restamped || got.Held || got.Err != nil {
					t.Fatalf("claude outcome = %+v, want no-op", got)
				}
				rec, recorded, err := ReadPayloadRecord(dir)
				if err != nil || !recorded || rec != tc.concurrent {
					t.Fatalf("record = %+v (recorded=%v, err=%v), want concurrent %+v", rec, recorded, err, tc.concurrent)
				}
			})
		}
	})

	t.Run("stamps legacy unstamped matching tree once", func(t *testing.T) {
		dirs, dirFor := makeAgentDirs(t.TempDir())
		dir := dirs[AgentAntigravity]
		if err := Extract(dir, v2FS); err != nil {
			t.Fatal(err)
		}

		outcomes := refreshByAgent(RefreshInstalled(ConsentEnv{
			FS:               v2FS,
			Record:           recV2Unattended,
			Manifest:         v2Manifest,
			SettingsReadable: true,
			DirFor:           dirFor,
		}))
		if got := outcomes[AgentAntigravity]; got.Updated || !got.Restamped || got.Held || got.Err != nil {
			t.Fatalf("legacy matching antigravity = %+v, want Restamped", got)
		}
		rec, recorded, err := ReadPayloadRecord(dir)
		if err != nil || !recorded || rec != recV2Unattended {
			t.Fatalf("record = %+v (recorded=%v, err=%v), want %+v", rec, recorded, err, recV2Unattended)
		}
	})
}
