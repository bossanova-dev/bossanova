package main

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/recurser/bossalib/revisiondrift"
)

func TestResolveEnvReport_EnvFirst(t *testing.T) {
	env := map[string]string{
		"BOSS_SESSION_ID":       "sess-123",
		"BOSS_AGENT_SESSION_ID": "chat-456",
		"BOSS_REPO_ID":          "repo-789",
		"BOSS_AGENT":            "codex",
		"BOSS_WORKTREE":         "/tmp/wt",
		"BOSS_SETTINGS_PATH":    "/cfg/settings.json",
		"BOSS_SOCKET":           "/cfg/bossd.sock",
		"BOSS_BIN":              "/usr/local/bin/boss",
		"BOSS_MCP_BIN":          "/usr/local/bin/mcp",
		"BOSS_CRON":             "true",
		"BOSS_CRON_JOB_ID":      "cron-1",
		"BOSS_CRON_NAME":        "bs-technical-debt",
	}
	got := resolveEnvReport(func(k string) string { return env[k] })

	if got.Session.SessionID != "sess-123" {
		t.Errorf("SessionID = %q, want sess-123", got.Session.SessionID)
	}
	if got.Session.Agent != "codex" {
		t.Errorf("Agent = %q, want codex", got.Session.Agent)
	}
	if got.Mode != "cron" {
		t.Errorf("Mode = %q, want cron", got.Mode)
	}
	if got.Cron == nil || got.Cron.Name != "bs-technical-debt" {
		t.Errorf("Cron not populated from env: %+v", got.Cron)
	}
	if got.Binaries.Boss != "/usr/local/bin/boss" {
		t.Errorf("Binaries.Boss = %q", got.Binaries.Boss)
	}
	if got.Binaries.SettingsPath != "/cfg/settings.json" {
		t.Errorf("SettingsPath = %q, want /cfg/settings.json (env-first)", got.Binaries.SettingsPath)
	}
	if got.Daemon.Socket != "/cfg/bossd.sock" {
		t.Errorf("Daemon.Socket = %q, want /cfg/bossd.sock (env-first)", got.Daemon.Socket)
	}
}

func TestResolveEnvReport_ManagedNonCron(t *testing.T) {
	env := map[string]string{
		"BOSS_SESSION_ID": "sess-123",
		"BOSS_AGENT":      "claude",
	}
	got := resolveEnvReport(func(k string) string { return env[k] })
	if got.Mode != "managed" {
		t.Errorf("Mode = %q, want managed", got.Mode)
	}
	if got.Cron != nil {
		t.Errorf("Cron should be nil for non-cron session, got %+v", got.Cron)
	}
}

func TestResolveEnvReport_StandaloneFallsBackToConfig(t *testing.T) {
	// No BOSS_* vars: not a managed session. SettingsPath/Socket fall back to
	// fresh config resolution; session identifiers are empty.
	got := resolveEnvReport(func(string) string { return "" })
	if got.Mode != "standalone" {
		t.Errorf("Mode = %q, want standalone", got.Mode)
	}
	if got.Session.SessionID != "" {
		t.Errorf("SessionID = %q, want empty in standalone mode", got.Session.SessionID)
	}
	// Fresh resolution should at least produce a settings path.
	if got.Binaries.SettingsPath == "" {
		t.Errorf("SettingsPath should be resolved from config in standalone mode")
	}
}

func TestResolveEnvReport_CapabilitiesPopulated(t *testing.T) {
	got := resolveEnvReport(func(string) string { return "" })
	if len(got.Capabilities.MCP) == 0 {
		t.Error("Capabilities.MCP should be non-empty")
	}
	foundLS := false
	for _, c := range got.Capabilities.CLI {
		if c == "boss ls" {
			foundLS = true
		}
	}
	if !foundLS {
		t.Errorf("Capabilities.CLI should include 'boss ls'; got %v", got.Capabilities.CLI)
	}
	foundListSessions := false
	for _, m := range got.Capabilities.MCP {
		if m == "list_sessions" {
			foundListSessions = true
		}
	}
	if !foundListSessions {
		t.Errorf("Capabilities.MCP should include 'list_sessions'; got %v", got.Capabilities.MCP)
	}
}

func TestRenderEnvHuman_ContainsKeySections(t *testing.T) {
	rep := resolveEnvReport(func(string) string { return "" })
	out := renderEnvHuman(rep)
	for _, want := range []string{"Mode:", "Capabilities", "CLI commands", "MCP tools"} {
		if !strings.Contains(out, want) {
			t.Errorf("human output missing %q\n---\n%s", want, out)
		}
	}
}

// --- BOS-1300: revision drift in `boss env` -----------------------------------

// stubEnvRevisionDrift pins the probe resolveEnvReport calls. Without it the
// pure, injectable core would shell out to the developer's real checkout, and
// these assertions would be decided by whether that machine has a stale build.
func stubEnvRevisionDrift(t *testing.T, drift revisiondrift.Drift) {
	t.Helper()
	previous := envRevisionDrift
	envRevisionDrift = func() revisiondrift.Drift { return drift }
	t.Cleanup(func() { envRevisionDrift = previous })
}

func behindEnvDrift() revisiondrift.Drift {
	return revisiondrift.Drift{
		Behind:           true,
		BehindKnown:      true,
		Reason:           revisiondrift.ReasonBehind,
		RevisionStamped:  true,
		BinaryRevision:   "ba75863ae",
		BinaryVersion:    "v2.1.197",
		CheckoutRoot:     "/src/bossanova",
		CheckoutRevision: "81f9ab4d9",
	}
}

func TestResolveEnvReport_CarriesTheRevisionRelation(t *testing.T) {
	stubEnvRevisionDrift(t, behindEnvDrift())

	got := resolveEnvReport(func(string) string { return "" })

	if got.Revision.Relation != "behind" {
		t.Errorf("Relation = %q, want behind", got.Revision.Relation)
	}
	if got.Revision.Reason != string(revisiondrift.ReasonBehind) {
		t.Errorf("Reason = %q, want %q", got.Revision.Reason, revisiondrift.ReasonBehind)
	}
	if got.Revision.BinaryRevision != "ba75863ae" || got.Revision.CheckoutRevision != "81f9ab4d9" {
		t.Errorf("both revisions should be reported: %+v", got.Revision)
	}
	if got.Revision.CheckoutRoot != "/src/bossanova" {
		t.Errorf("CheckoutRoot = %q, want the checkout it compared against", got.Revision.CheckoutRoot)
	}
}

func TestResolveEnvReport_DescendantRelation(t *testing.T) {
	drift := behindEnvDrift()
	drift.Behind = false
	drift.Reason = revisiondrift.ReasonDescendant
	stubEnvRevisionDrift(t, drift)

	if got := resolveEnvReport(func(string) string { return "" }); got.Revision.Relation != "descendant" {
		t.Fatalf("Relation = %q, want descendant", got.Revision.Relation)
	}
}

// TestResolveEnvReport_UnknownRevisionCausesStayDistinct pins that env does not
// flatten the could-not-evaluate causes into one shared "unknown". The relation
// is unknown for all of them, which is exactly why the reason has to carry
// which one — a consumer reading only the relation cannot tell "no checkout"
// from "no git", and those have different fixes.
func TestResolveEnvReport_UnknownRevisionCausesStayDistinct(t *testing.T) {
	reasons := []revisiondrift.Reason{
		revisiondrift.ReasonNoCheckout,
		revisiondrift.ReasonUnstamped,
		revisiondrift.ReasonDevBuild,
		revisiondrift.ReasonRevisionAbsent,
		revisiondrift.ReasonCheckoutUntrusted,
		revisiondrift.ReasonGitUnavailable,
	}
	seen := map[string]bool{}
	for _, reason := range reasons {
		drift := revisiondrift.Drift{Reason: reason, RevisionStamped: reason != revisiondrift.ReasonUnstamped}
		stubEnvRevisionDrift(t, drift)

		got := resolveEnvReport(func(string) string { return "" }).Revision
		if got.Relation != "unknown" {
			t.Errorf("%q: Relation = %q, want unknown", reason, got.Relation)
		}
		if got.Reason != string(reason) {
			t.Errorf("%q: Reason = %q, want the cause carried through", reason, got.Reason)
		}
		if seen[got.Reason] {
			t.Errorf("%q: two causes rendered the same reason %q", reason, got.Reason)
		}
		seen[got.Reason] = true
	}
	if len(seen) != len(reasons) {
		t.Fatalf("%d distinct reasons for %d causes — some were flattened", len(seen), len(reasons))
	}
}

// TestResolveEnvReport_DisplaySentinelStaysOutOfTheSchema pins the split the
// --json schema needs: the machine-readable revision is the real revision or
// empty, and the parenthesised sentinel lives only in the human rendering.
//
// A sentinel in the schema makes a consumer string-match prose to learn what
// `relation` and `reason` already carry as data, and makes this the only field
// whose emptiness is spelled as a word.
func TestResolveEnvReport_DisplaySentinelStaysOutOfTheSchema(t *testing.T) {
	for _, tc := range []struct {
		name      string
		drift     revisiondrift.Drift
		wantJSON  string
		wantHuman string
	}{
		{
			name:      "unstamped",
			drift:     revisiondrift.Drift{Reason: revisiondrift.ReasonUnstamped},
			wantJSON:  "",
			wantHuman: "(unstamped)",
		},
		{
			// The no-ldflags default is a producer state, not a revision
			// named "unknown" — so it is absent from the schema too.
			name:      "unknown default",
			drift:     revisiondrift.Drift{Reason: revisiondrift.ReasonUnstamped, BinaryRevision: "unknown"},
			wantJSON:  "",
			wantHuman: "(unstamped)",
		},
		{
			name:      "unreadable",
			drift:     revisiondrift.Unknown(revisiondrift.ReasonRevisionUnreadable, "boom"),
			wantJSON:  "",
			wantHuman: "(unreadable)",
		},
		{
			name:      "stamped",
			drift:     revisiondrift.Drift{Reason: revisiondrift.ReasonDescendant, BehindKnown: true, RevisionStamped: true, BinaryRevision: "ba75863ae"},
			wantJSON:  "ba75863ae",
			wantHuman: "ba75863ae",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stubEnvRevisionDrift(t, tc.drift)
			rep := resolveEnvReport(func(string) string { return "" })

			if got := rep.Revision.BinaryRevision; got != tc.wantJSON {
				t.Errorf("schema BinaryRevision = %q, want %q", got, tc.wantJSON)
			}
			// Serialised, because the schema is what a consumer reads.
			encoded, err := json.Marshal(rep.Revision)
			if err != nil {
				t.Fatalf("marshal revision: %v", err)
			}
			for _, sentinel := range []string{"(unstamped)", "(unreadable)", "(unknown)"} {
				if strings.Contains(string(encoded), sentinel) {
					t.Errorf("--json carries the display sentinel %s: %s", sentinel, encoded)
				}
			}
			if got := envHumanRevisionLabel(rep.Revision); got != tc.wantHuman {
				t.Errorf("human label = %q, want %q", got, tc.wantHuman)
			}
			if want := "boss revision:      " + tc.wantHuman; !strings.Contains(renderEnvHuman(rep), want) {
				t.Errorf("human rendering missing %q:\n%s", want, renderEnvHuman(rep))
			}
		})
	}
}

func TestRenderEnvHuman_IncludesTheRevisionSection(t *testing.T) {
	stubEnvRevisionDrift(t, behindEnvDrift())
	rendered := renderEnvHuman(resolveEnvReport(func(string) string { return "" }))

	for _, want := range []string{
		"\nRevision:",
		"boss revision:",
		"checkout revision:",
		"relation:           behind",
		string(revisiondrift.ReasonBehind),
		"ba75863ae",
		"81f9ab4d9",
	} {
		if !strings.Contains(rendered, want) {
			t.Errorf("human rendering missing %q:\n%s", want, rendered)
		}
	}
}

// TestEnvJSON_AddsRevisionWithoutRenamingAnyField is the additive half of the
// contract. EnvReport documents its field names as part of its schema, so the
// assertion is on the marshalled JSON keys: the new one is present AND every
// pre-existing one this change sat next to still is.
func TestEnvJSON_AddsRevisionWithoutRenamingAnyField(t *testing.T) {
	stubEnvRevisionDrift(t, behindEnvDrift())
	encoded, err := json.Marshal(resolveEnvReport(func(string) string { return "" }))
	if err != nil {
		t.Fatalf("marshal env report: %v", err)
	}

	var decoded map[string]json.RawMessage
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatalf("unmarshal env report: %v", err)
	}
	for _, key := range []string{"mode", "profile", "session", "binaries", "daemon", "capabilities", "revision"} {
		if _, ok := decoded[key]; !ok {
			t.Errorf("env JSON has no %q key: %s", key, encoded)
		}
	}

	var binaries map[string]json.RawMessage
	if err := json.Unmarshal(decoded["binaries"], &binaries); err != nil {
		t.Fatalf("unmarshal binaries: %v", err)
	}
	for _, key := range []string{"settings_path", "boss", "mcp"} {
		if _, ok := binaries[key]; !ok {
			t.Errorf("binaries.%s was renamed — that is a breaking change: %s", key, decoded["binaries"])
		}
	}

	var revision map[string]json.RawMessage
	if err := json.Unmarshal(decoded["revision"], &revision); err != nil {
		t.Fatalf("unmarshal revision: %v", err)
	}
	for _, key := range []string{
		"binary_version", "binary_revision", "checkout_root", "checkout_revision", "relation", "reason",
	} {
		if _, ok := revision[key]; !ok {
			t.Errorf("revision JSON has no %q key: %s", key, decoded["revision"])
		}
	}
	// A zero-valued Revision struct still marshals every key above, so key
	// presence alone would pass on a report that was never populated. The
	// relation is the field with no valid empty value.
	if string(revision["relation"]) != `"behind"` {
		t.Fatalf("revision.relation = %s, want \"behind\" — the report was not populated", revision["relation"])
	}
}
