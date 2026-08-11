package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/recurser/boss/internal/client"
)

// agentsListStub is the narrow ListAgents seam renderAgents consumes, following
// the agentPreflightStub pattern in handlers_test.go: the command is exercised
// against a stub client rather than a live daemon, so these tests assert the
// rendering contract and nothing about transport.
type agentsListStub struct {
	agents []client.AgentInfo
	err    error
	calls  int
}

func (s *agentsListStub) ListAgents(context.Context) ([]client.AgentInfo, error) {
	s.calls++
	if s.err != nil {
		return nil, s.err
	}
	return s.agents, nil
}

// newAgentsTestCmd builds the real `boss agents` command with its output
// redirected, so the flag set under test is the one the CLI registers rather
// than a fixture that could drift from it.
func newAgentsTestCmd(t *testing.T) (*cobra.Command, *bytes.Buffer) {
	t.Helper()
	cmd := agentsCmd()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	return cmd, &out
}

// decodeAgentsEnvelope unmarshals the success envelope. Asserting by decoding —
// never by substring — is what makes these tests a contract check rather than a
// formatting check: a renamed key or a numeric enum fails here, while a
// substring assertion on `"name": "claude"` would survive both.
func decodeAgentsEnvelope(t *testing.T, raw string) agentsJSON {
	t.Helper()
	var got agentsJSON
	if err := json.Unmarshal([]byte(raw), &got); err != nil {
		t.Fatalf("unmarshal agents envelope: %v\nraw output:\n%s", err, raw)
	}
	return got
}

func TestRunAgentsTableRendersNameAndVersion(t *testing.T) {
	cmd, out := newAgentsTestCmd(t)
	stub := &agentsListStub{agents: []client.AgentInfo{
		{Name: "claude", Version: "1.2.3", UserSettings: []client.UserSetting{{Key: "model"}}},
		{Name: "codex", Version: "0.9.0"},
	}}

	if err := renderAgents(cmd, stub, false); err != nil {
		t.Fatalf("renderAgents: %v", err)
	}
	if stub.calls != 1 {
		t.Fatalf("ListAgents calls = %d, want 1", stub.calls)
	}

	view := out.String()
	for _, want := range []string{"NAME", "VERSION", "SETTINGS", "claude", "1.2.3", "codex", "0.9.0"} {
		if !strings.Contains(view, want) {
			t.Errorf("table output missing %q:\n%s", want, view)
		}
	}
	// SETTINGS is a count, not the settings themselves — the detail belongs in
	// --json rather than a wrapped table cell.
	if strings.Contains(view, "model") {
		t.Errorf("table rendered setting detail; SETTINGS should be a count:\n%s", view)
	}
}

func TestRunAgentsTableWithNoAgents(t *testing.T) {
	cmd, out := newAgentsTestCmd(t)
	if err := renderAgents(cmd, &agentsListStub{}, false); err != nil {
		t.Fatalf("renderAgents: %v", err)
	}
	if got := out.String(); !strings.Contains(got, "No agent runners loaded.") {
		t.Errorf("empty table output = %q, want the no-agents notice", got)
	}
}

func TestRunAgentsJSONEmitsFullAgentShape(t *testing.T) {
	cmd, out := newAgentsTestCmd(t)
	stub := &agentsListStub{agents: []client.AgentInfo{{
		Name:    "claude",
		Version: "1.2.3",
		UserSettings: []client.UserSetting{{
			Key:           "model",
			Label:         "Model",
			Description:   "Which model to run",
			DefaultValue:  "opus",
			Type:          client.SettingTypeEnum,
			AllowedValues: []string{"opus", "sonnet"},
		}},
	}}}

	if err := renderAgents(cmd, stub, true); err != nil {
		t.Fatalf("renderAgents: %v", err)
	}

	got := decodeAgentsEnvelope(t, out.String())
	if len(got.Agents) != 1 {
		t.Fatalf("agents length = %d, want 1 (%+v)", len(got.Agents), got.Agents)
	}
	agent := got.Agents[0]
	if agent.Name != "claude" || agent.Version != "1.2.3" {
		t.Errorf("agent = %+v, want name=claude version=1.2.3", agent)
	}
	if len(agent.UserSettings) != 1 {
		t.Fatalf("user_settings length = %d, want 1", len(agent.UserSettings))
	}
	want := userSettingJSON{
		Key:           "model",
		Label:         "Model",
		Description:   "Which model to run",
		DefaultValue:  "opus",
		Type:          "ENUM",
		AllowedValues: []string{"opus", "sonnet"},
	}
	setting := agent.UserSettings[0]
	if setting.Key != want.Key || setting.Label != want.Label ||
		setting.Description != want.Description || setting.DefaultValue != want.DefaultValue ||
		setting.Type != want.Type || strings.Join(setting.AllowedValues, ",") != strings.Join(want.AllowedValues, ",") {
		t.Errorf("user_settings[0] = %+v, want %+v", setting, want)
	}
}

// TestRunAgentsJSONTypeIsEnumName pins every SettingType to its wire spelling.
// The local SettingType is an int whose ordering is an implementation detail; a
// caller must never have to know that Bool happens to be 1.
func TestRunAgentsJSONTypeIsEnumName(t *testing.T) {
	cases := []struct {
		typ  client.SettingType
		want string
	}{
		{client.SettingTypeUnspecified, "UNSPECIFIED"},
		{client.SettingTypeBool, "BOOL"},
		{client.SettingTypeString, "STRING"},
		{client.SettingTypeEnum, "ENUM"},
		// An int outside the enum must still render a name, never a number.
		{client.SettingType(99), "UNSPECIFIED"},
	}
	for _, tc := range cases {
		t.Run(tc.want, func(t *testing.T) {
			cmd, out := newAgentsTestCmd(t)
			stub := &agentsListStub{agents: []client.AgentInfo{{
				Name:         "claude",
				UserSettings: []client.UserSetting{{Key: "k", Type: tc.typ}},
			}}}
			if err := renderAgents(cmd, stub, true); err != nil {
				t.Fatalf("renderAgents: %v", err)
			}
			got := decodeAgentsEnvelope(t, out.String())
			if len(got.Agents) != 1 || len(got.Agents[0].UserSettings) != 1 {
				t.Fatalf("unexpected envelope: %+v", got)
			}
			if gotType := got.Agents[0].UserSettings[0].Type; gotType != tc.want {
				t.Errorf("type = %q, want %q", gotType, tc.want)
			}
		})
	}
}

// TestRunAgentsJSONEmptySettingsIsEmptyArray is the null-vs-[] guard. It reads
// the raw bytes rather than the decoded struct on purpose: `null` and `[]` both
// decode to a zero-length slice, so a decoded-only assertion would pass against
// the naive nil-slice implementation this test exists to reject.
func TestRunAgentsJSONEmptySettingsIsEmptyArray(t *testing.T) {
	cmd, out := newAgentsTestCmd(t)
	stub := &agentsListStub{agents: []client.AgentInfo{{Name: "claude", Version: "1.2.3"}}}
	if err := renderAgents(cmd, stub, true); err != nil {
		t.Fatalf("renderAgents: %v", err)
	}

	// Decode into json.RawMessage so the emitted token itself is observable.
	var envelope struct {
		Agents []struct {
			UserSettings json.RawMessage `json:"user_settings"`
		} `json:"agents"`
	}
	raw := out.String()
	if err := json.Unmarshal([]byte(raw), &envelope); err != nil {
		t.Fatalf("unmarshal: %v\nraw output:\n%s", err, raw)
	}
	if len(envelope.Agents) != 1 {
		t.Fatalf("agents length = %d, want 1", len(envelope.Agents))
	}
	if got := strings.TrimSpace(string(envelope.Agents[0].UserSettings)); got != "[]" {
		t.Errorf("user_settings = %s, want [] (a driver iterating it must not have to null-check)", got)
	}
}

// TestRunAgentsJSONAllowedValuesIsEmptyArray applies the same rule one level
// down: allowed_values is iterated by the same drivers for the same reason.
func TestRunAgentsJSONAllowedValuesIsEmptyArray(t *testing.T) {
	cmd, out := newAgentsTestCmd(t)
	stub := &agentsListStub{agents: []client.AgentInfo{{
		Name:         "claude",
		UserSettings: []client.UserSetting{{Key: "verbose", Type: client.SettingTypeBool}},
	}}}
	if err := renderAgents(cmd, stub, true); err != nil {
		t.Fatalf("renderAgents: %v", err)
	}

	var envelope struct {
		Agents []struct {
			UserSettings []struct {
				AllowedValues json.RawMessage `json:"allowed_values"`
			} `json:"user_settings"`
		} `json:"agents"`
	}
	raw := out.String()
	if err := json.Unmarshal([]byte(raw), &envelope); err != nil {
		t.Fatalf("unmarshal: %v\nraw output:\n%s", err, raw)
	}
	if len(envelope.Agents) != 1 || len(envelope.Agents[0].UserSettings) != 1 {
		t.Fatalf("unexpected envelope shape: %s", raw)
	}
	if got := strings.TrimSpace(string(envelope.Agents[0].UserSettings[0].AllowedValues)); got != "[]" {
		t.Errorf("allowed_values = %s, want []", got)
	}
}

// TestRunAgentsJSONZeroAgents pins the empty fleet as a valid answer: an
// envelope with an empty array and a zero exit, not an error.
func TestRunAgentsJSONZeroAgents(t *testing.T) {
	cmd, out := newAgentsTestCmd(t)
	if err := renderAgents(cmd, &agentsListStub{}, true); err != nil {
		t.Fatalf("renderAgents returned an error for an empty fleet: %v", err)
	}

	raw := out.String()
	var envelope struct {
		Agents json.RawMessage `json:"agents"`
	}
	if err := json.Unmarshal([]byte(raw), &envelope); err != nil {
		t.Fatalf("unmarshal: %v\nraw output:\n%s", err, raw)
	}
	if got := strings.TrimSpace(string(envelope.Agents)); got != "[]" {
		t.Errorf("agents = %s, want []", got)
	}
	if got := decodeAgentsEnvelope(t, raw); len(got.Agents) != 0 {
		t.Errorf("agents length = %d, want 0", len(got.Agents))
	}
}

// TestRunAgentsListErrorEmitsEnvelope covers the failure exit: the shared error
// envelope on stdout AND a returned error, which is what makes main exit 1.
func TestRunAgentsListErrorEmitsEnvelope(t *testing.T) {
	cmd, out := newAgentsTestCmd(t)
	stub := &agentsListStub{err: errors.New("daemon unreachable")}

	err := renderAgents(cmd, stub, true)
	if err == nil {
		t.Fatal("renderAgents returned nil for a failing ListAgents; the CLI would exit 0")
	}

	var envelope jsonErrorEnvelope
	raw := out.String()
	if unmarshalErr := json.Unmarshal([]byte(raw), &envelope); unmarshalErr != nil {
		t.Fatalf("unmarshal error envelope: %v\nraw output:\n%s", unmarshalErr, raw)
	}
	if envelope.Error.Code != "UNKNOWN" {
		t.Errorf("error.code = %q, want UNKNOWN", envelope.Error.Code)
	}
	if envelope.Error.ConnectCode == "" {
		t.Error("error.connect_code is empty; a driver meeting an unfamiliar code has no fallback")
	}
	if !strings.Contains(envelope.Error.Message, "daemon unreachable") {
		t.Errorf("error.message = %q, want it to carry the underlying failure", envelope.Error.Message)
	}
}

// TestRunAgentsListErrorWithoutJSON keeps the human path unchanged: no envelope
// on stdout, the error returned for main's stderr line.
func TestRunAgentsListErrorWithoutJSON(t *testing.T) {
	cmd, out := newAgentsTestCmd(t)
	err := renderAgents(cmd, &agentsListStub{err: errors.New("daemon unreachable")}, false)
	if err == nil {
		t.Fatal("renderAgents returned nil for a failing ListAgents")
	}
	if got := out.String(); strings.Contains(got, "\"error\"") {
		t.Errorf("human path emitted a JSON envelope: %q", got)
	}
}

// TestRunAgentsCommandIsRegistered proves the command reaches `boss --help`
// with a --json flag, rather than only being reachable through renderAgents.
func TestRunAgentsCommandIsRegistered(t *testing.T) {
	root := rootCmd()
	found, _, err := root.Find([]string{"agents"})
	if err != nil {
		t.Fatalf("root.Find(agents): %v", err)
	}
	if found == nil || found.Name() != "agents" {
		t.Fatalf("`boss agents` is not registered on the root command (found %v)", found)
	}
	if found.Hidden {
		t.Error("`boss agents` is hidden; it must appear in `boss --help`")
	}
	// Assert the concrete group, not merely a non-empty one: a typo'd GroupID
	// would satisfy `!= ""` while silently filing the command away from
	// `boss plugin list`, which is the whole point of the placement.
	plugin, _, err := root.Find([]string{"plugin"})
	if err != nil {
		t.Fatalf("root.Find(plugin): %v", err)
	}
	if found.GroupID != plugin.GroupID {
		t.Errorf("`boss agents` is in group %q, want %q (alongside `boss plugin list`)",
			found.GroupID, plugin.GroupID)
	}
	if !slices.ContainsFunc(root.Groups(), func(g *cobra.Group) bool { return g.ID == found.GroupID }) {
		t.Errorf("`boss agents` GroupID %q is not a registered group", found.GroupID)
	}
	if found.Flags().Lookup(jsonFlagName) == nil {
		t.Error("`boss agents` does not register --json")
	}
}

// TestRunAgentsJSONFlagDrivesOutput covers the argv→output wiring that every
// other test skips by calling renderAgents with an explicit bool. It parses real
// CLI arguments against the real command's flag set, so a flag registered under
// the wrong name, or one runAgents never reads, changes the rendered output here.
func TestRunAgentsJSONFlagDrivesOutput(t *testing.T) {
	agents := []client.AgentInfo{{Name: "claude", Version: "1.2.3"}}

	for _, tc := range []struct {
		name     string
		argv     []string
		wantJSON bool
	}{
		{name: "no flag renders the table", argv: nil, wantJSON: false},
		{name: "--json renders JSON", argv: []string{"--" + jsonFlagName}, wantJSON: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cmd, out := newAgentsTestCmd(t)
			if err := cmd.ParseFlags(tc.argv); err != nil {
				t.Fatalf("ParseFlags(%v): %v", tc.argv, err)
			}
			asJSON, err := cmd.Flags().GetBool(jsonFlagName)
			if err != nil {
				t.Fatalf("get --%s: %v", jsonFlagName, err)
			}
			if err := renderAgents(cmd, &agentsListStub{agents: agents}, asJSON); err != nil {
				t.Fatalf("renderAgents: %v", err)
			}

			var decoded agentsJSON
			gotJSON := json.Unmarshal(out.Bytes(), &decoded) == nil
			if gotJSON != tc.wantJSON {
				t.Errorf("output parsed as JSON = %v, want %v (output %q)", gotJSON, tc.wantJSON, out.String())
			}
		})
	}
}
