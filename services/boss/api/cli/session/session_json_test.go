package session

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/recurser/boss/internal/domain/rpc"

	"github.com/spf13/cobra"

	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func TestSessionRowJSONMarshalsLivenessAndTrackerFields(t *testing.T) {
	trackerID := "BOS-908"
	prNumber := int32(42)
	prURL := "https://github.com/acme/app/pull/42"
	session := &pb.Session{
		Id:                  "sess-1",
		Title:               "ship liveness fields",
		State:               pb.SessionState_SESSION_STATE_READY_FOR_REVIEW,
		RepoId:              "repo-1",
		AgentName:           "codex",
		EffectiveModel:      "gpt-5-codex",
		EffectiveEffort:     "medium",
		PrNumber:            &prNumber,
		PrUrl:               &prURL,
		BranchName:          "bos-908",
		CreatedAt:           timestamppb.New(time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)),
		UpdatedAt:           timestamppb.New(time.Date(2026, 1, 2, 4, 5, 6, 0, time.UTC)),
		TrackerId:           &trackerID,
		LastAgentActivityAt: timestamppb.New(time.Date(2026, 1, 2, 5, 6, 7, 0, time.UTC)),
	}

	got, err := json.Marshal(newSessionRowJSON(session))
	if err != nil {
		t.Fatalf("json.Marshal(newSessionRowJSON()) error = %v", err)
	}

	for _, want := range []string{
		`"tracker_id":"BOS-908"`,
		`"last_agent_activity_at":"2026-01-02T05:06:07Z"`,
	} {
		if !strings.Contains(string(got), want) {
			t.Fatalf("marshalled row = %s, want it to contain %s", got, want)
		}
	}
}

func TestSessionRowJSONMarshalsAbsentTrackerAndActivity(t *testing.T) {
	got, err := json.Marshal(newSessionRowJSON(&pb.Session{}))
	if err != nil {
		t.Fatalf("json.Marshal(newSessionRowJSON()) error = %v", err)
	}

	for _, want := range []string{
		`"tracker_id":null`,
		`"last_agent_activity_at":""`,
		`"created_at":""`,
		`"updated_at":""`,
	} {
		if !strings.Contains(string(got), want) {
			t.Fatalf("marshalled row = %s, want it to contain %s", got, want)
		}
	}
}

func TestSessionRowJSONKeySet(t *testing.T) {
	trackerID := "BOS-908"
	prNumber := int32(42)
	prURL := "https://github.com/acme/app/pull/42"
	session := &pb.Session{
		Id:                  "sess-1",
		Title:               "ship liveness fields",
		State:               pb.SessionState_SESSION_STATE_READY_FOR_REVIEW,
		RepoId:              "repo-1",
		AgentName:           "codex",
		EffectiveModel:      "gpt-5-codex",
		EffectiveEffort:     "medium",
		PrNumber:            &prNumber,
		PrUrl:               &prURL,
		BranchName:          "bos-908",
		CreatedAt:           timestamppb.New(time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)),
		UpdatedAt:           timestamppb.New(time.Date(2026, 1, 2, 4, 5, 6, 0, time.UTC)),
		TrackerId:           &trackerID,
		LastAgentActivityAt: timestamppb.New(time.Date(2026, 1, 2, 5, 6, 7, 0, time.UTC)),
	}

	got, err := json.Marshal(newSessionRowJSON(session))
	if err != nil {
		t.Fatalf("json.Marshal(newSessionRowJSON()) error = %v", err)
	}

	var fields map[string]json.RawMessage
	if err := json.Unmarshal(got, &fields); err != nil {
		t.Fatalf("json.Unmarshal(row) error = %v; row = %s", err, got)
	}

	wantFields := map[string]string{
		"id":                     `"sess-1"`,
		"title":                  `"ship liveness fields"`,
		"state":                  `"READY_FOR_REVIEW"`,
		"repo_id":                `"repo-1"`,
		"agent":                  `"codex"`,
		"model":                  `"gpt-5-codex"`,
		"effort":                 `"medium"`,
		"pr_number":              `42`,
		"pr_url":                 `"https://github.com/acme/app/pull/42"`,
		"branch":                 `"bos-908"`,
		"created_at":             `"2026-01-02T03:04:05Z"`,
		"updated_at":             `"2026-01-02T04:05:06Z"`,
		"tracker_id":             `"BOS-908"`,
		"last_agent_activity_at": `"2026-01-02T05:06:07Z"`,
	}
	if len(fields) != len(wantFields) {
		t.Fatalf("keys = %v, want exactly %v", sortedJSONKeys(fields), sortedStringKeys(wantFields))
	}
	for key, want := range wantFields {
		got := string(fields[key])
		if got == "" {
			t.Fatalf("keys = %v, missing %q", sortedJSONKeys(fields), key)
		}
		if got != want {
			t.Fatalf("%s = %s, want %s", key, got, want)
		}
	}
}

func sortedJSONKeys(fields map[string]json.RawMessage) []string {
	keys := make([]string, 0, len(fields))
	for key := range fields {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func sortedStringKeys(fields map[string]string) []string {
	keys := make([]string, 0, len(fields))
	for key := range fields {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

// TestSessionDetailJSONCarriesBlockedReason pins the machine-readable half of
// the 2026-09-03 fix. `boss show --json` is what a driver or a follow-up agent
// reads, and before this the blocked reason reached no CLI surface at all.
func TestSessionDetailJSONCarriesBlockedReason(t *testing.T) {
	const reason = "draft PR creation failed: create PR: HTTP 401: Requires authentication\nTry authenticating with:  gh auth login"
	detail := newSessionDetailJSON(&pb.Session{Id: "sess-1", BlockedReason: &[]string{reason}[0]})

	if detail.BlockedReason == nil {
		t.Fatal("BlockedReason = nil, want the reason")
	}
	if *detail.BlockedReason != reason {
		t.Fatalf("BlockedReason = %q, want %q — it must be verbatim and untruncated", *detail.BlockedReason, reason)
	}

	encoded, err := json.Marshal(detail)
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}
	if !strings.Contains(string(encoded), `"blocked_reason":`) {
		t.Fatalf("encoded detail has no blocked_reason key: %s", encoded)
	}
}

// TestSessionDetailJSONBlockedReasonIsNullWhenUnblocked pins the pointer choice.
// null ("not blocked") must stay distinguishable from "" so a driver can tell
// the two apart, which is why the field is not a bare string.
func TestSessionDetailJSONBlockedReasonIsNullWhenUnblocked(t *testing.T) {
	detail := newSessionDetailJSON(&pb.Session{Id: "sess-1"})
	if detail.BlockedReason != nil {
		t.Fatalf("BlockedReason = %q, want nil", *detail.BlockedReason)
	}
	encoded, err := json.Marshal(detail)
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}
	if !strings.Contains(string(encoded), `"blocked_reason":null`) {
		t.Fatalf("encoded detail did not carry an explicit null: %s", encoded)
	}
}

// TestSessionRowJSONOmitsBlockedReason records the deliberate decision that
// `boss ls --json` does NOT carry it: that row shape is the widest contract in
// the file, and a raw multi-line daemon error on every row is noise for a list.
// Detail is where triage happens.
func TestSessionRowJSONOmitsBlockedReason(t *testing.T) {
	row := newSessionRowJSON(&pb.Session{Id: "sess-1", BlockedReason: &[]string{"draft PR creation failed: boom"}[0]})
	encoded, err := json.Marshal(row)
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}
	if strings.Contains(string(encoded), "blocked_reason") {
		t.Fatalf("the ls row grew a blocked_reason field: %s", encoded)
	}
}

func TestSessionDetailJSONArchiveAfterMerge(t *testing.T) {
	for _, tt := range []struct {
		name        string
		displayName string
		archive     bool
		want        string
	}{
		{name: "hydrated true", displayName: "app", archive: true, want: "true"},
		{name: "hydrated false", displayName: "app", want: "false"},
		{name: "hydration missing", want: "null"},
		{name: "flag without hydration", archive: true, want: "null"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			encoded, err := json.Marshal(newSessionDetailJSON(&pb.Session{
				RepoDisplayName:                     tt.displayName,
				RepoShouldArchiveSessionsAfterMerge: tt.archive,
			}))
			if err != nil {
				t.Fatal(err)
			}
			var fields map[string]json.RawMessage
			if err := json.Unmarshal(encoded, &fields); err != nil {
				t.Fatal(err)
			}
			if got := string(fields["repo_should_archive_sessions_after_merge"]); got != tt.want {
				t.Fatalf("archive-after-merge = %s, want %s", got, tt.want)
			}
		})
	}
}

func TestSessionShowJSONRepairActive_Sessions_AC144(t *testing.T) {
	for _, tt := range []struct {
		name   string
		active bool
		want   string
	}{
		{name: "repair in flight", active: true, want: "true"},
		{name: "repair idle", active: false, want: "false"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			encoded, err := json.Marshal(sessionShowJSON{
				Session: newSessionDetailJSON(&pb.Session{RepairActive: tt.active}),
			})
			if err != nil {
				t.Fatal(err)
			}
			var envelope struct {
				Session map[string]json.RawMessage `json:"session"`
			}
			if err := json.Unmarshal(encoded, &envelope); err != nil {
				t.Fatal(err)
			}
			if got := string(envelope.Session["repair_active"]); got != tt.want {
				t.Fatalf("session.repair_active = %s, want %s", got, tt.want)
			}
		})
	}
}

func TestSessionDetailJSONRepoCanAutoRepair(t *testing.T) {
	for _, tt := range []struct {
		name        string
		displayName string
		autoRepair  bool
		want        string
	}{
		{name: "hydrated true", displayName: "app", autoRepair: true, want: "true"},
		{name: "hydrated false", displayName: "app", want: "false"},
		{name: "hydration missing", want: "null"},
		{name: "flag without hydration", autoRepair: true, want: "null"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			encoded, err := json.Marshal(newSessionDetailJSON(&pb.Session{
				RepoDisplayName:   tt.displayName,
				RepoCanAutoRepair: tt.autoRepair,
			}))
			if err != nil {
				t.Fatal(err)
			}
			var fields map[string]json.RawMessage
			if err := json.Unmarshal(encoded, &fields); err != nil {
				t.Fatal(err)
			}
			if got := string(fields["repo_can_auto_repair"]); got != tt.want {
				t.Fatalf("repo_can_auto_repair = %s, want %s", got, tt.want)
			}
		})
	}
}

type fleetListStub struct {
	Client
	req     *pb.ListSessionsRequest
	failure error
}

func (f *fleetListStub) ListSessionsWithReadFailures(_ context.Context, req *pb.ListSessionsRequest, _ rpc.SessionReadOptions) ([]*pb.Session, []*pb.OrganizationSessionReadFailure, error) {
	f.req = req
	url := "https://github.com/acme/app/pull/42"
	return []*pb.Session{{Id: "fleet-session", PrUrl: &url}}, []*pb.OrganizationSessionReadFailure{{OrganizationId: "org-unreadable", Reason: "offline"}}, f.failure
}

func executeSessionList(g *Group, args ...string) (string, error) {
	root := &cobra.Command{Use: "boss", SilenceUsage: true, SilenceErrors: true}
	root.PersistentFlags().String("remote", "", "")
	root.PersistentFlags().String("host", "", "")
	root.AddCommand(g.LSCmd())
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&bytes.Buffer{})
	root.SetArgs(append([]string{"ls"}, args...))
	err := root.Execute()
	return out.String(), err
}

func TestLSAllDaemonsReadsFleetWithFailures(t *testing.T) {
	t.Setenv("BOSS_CLOUD_URL", "https://fleet.example.test")
	stub := &fleetListStub{}
	g := testGroup()
	g.NewRemoteClient = func(_ *cobra.Command, url string) (Client, error) {
		if url != "https://fleet.example.test" {
			t.Fatalf("remote URL = %q", url)
		}
		return stub, nil
	}
	out, err := executeSessionList(g, "--all-daemons", "--json", "--repo", "repo-1", "--state", "implementing_plan", "--archived")
	if err != nil {
		t.Fatal(err)
	}
	if stub.req == nil || stub.req.GetRepoId() != "repo-1" || !stub.req.IncludeArchived || len(stub.req.States) != 1 || stub.req.States[0] != pb.SessionState_SESSION_STATE_IMPLEMENTING_PLAN {
		t.Fatalf("request = %v", stub.req)
	}
	var got struct {
		Sessions []struct {
			ID    string `json:"id"`
			PrURL string `json:"pr_url"`
		} `json:"sessions"`
		Failed []struct {
			OrganizationID string `json:"organization_id"`
			Reason         string `json:"reason"`
		} `json:"failed_organizations"`
	}
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Sessions) != 1 || got.Sessions[0].ID != "fleet-session" || got.Sessions[0].PrURL != "https://github.com/acme/app/pull/42" || len(got.Failed) != 1 || got.Failed[0].OrganizationID != "org-unreadable" || got.Failed[0].Reason != "offline" {
		t.Fatalf("fleet output = %s", out)
	}
	stub.failure = errors.New("fleet unavailable")
	out, err = executeSessionList(g, "--all-daemons", "--json")
	if err == nil || !strings.Contains(out, "fleet unavailable") {
		t.Fatalf("failed read: output %s, err %v", out, err)
	}
}

func TestLSAllDaemonsRejectsOtherSources(t *testing.T) {
	g := testGroup()
	g.NewRemoteClient = func(*cobra.Command, string) (Client, error) {
		t.Fatal("conflicting flags must not read sessions")
		return nil, nil
	}
	for _, flag := range []string{"--remote", "--host"} {
		t.Run(flag, func(t *testing.T) {
			out, err := executeSessionList(g, "--all-daemons", "--json", flag, "elsewhere")
			if err == nil || !strings.Contains(err.Error(), "--all-daemons cannot be combined with") || strings.Contains(out, `"sessions"`) {
				t.Fatalf("output %s, err %v", out, err)
			}
		})
	}
}

func TestLSLocalJSONKeepsEnvelope(t *testing.T) {
	daemon := newSessionDaemonFake(t)
	daemon.AddSession(&pb.Session{Id: "local-session"})
	t.Setenv("BOSS_SOCKET", daemon.SocketPath())
	g := testGroup()
	g.NewRemoteClient = func(*cobra.Command, string) (Client, error) {
		t.Fatal("local read used orchestrator")
		return nil, nil
	}
	out, err := executeSessionList(g, "--json")
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]json.RawMessage
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatal(err)
	}
	var sessions []sessionJSON
	if err := json.Unmarshal(got["sessions"], &sessions); err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || len(sessions) != 1 || sessions[0].ID != "local-session" {
		t.Fatalf("local output = %s", out)
	}
}
