package main

import (
	"encoding/json"
	"sort"
	"strings"
	"testing"
	"time"

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
