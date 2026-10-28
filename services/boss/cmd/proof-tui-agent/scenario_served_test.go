package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/recurser/boss/internal/adapter/realstackrpc"
	"github.com/recurser/boss/internal/scenario"
	"github.com/recurser/bossalib/displaystatus"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
)

// Run in full with: go test ./services/boss/cmd/proof-tui-agent -run TestServedScenarioDisplay.
func TestServedScenarioDisplay(t *testing.T) {
	if testing.Short() {
		t.Skip("real daemon integration; run go test ./services/boss/cmd/proof-tui-agent -run TestServedScenarioDisplay")
	}
	stack := startFixtureStack(t).stack
	doc := scenario.Document{Version: 1, Name: "served", Repos: []scenario.Repo{{Key: "r", DisplayName: "Scenario"}}, Sessions: []scenario.Session{{Key: "blocked", Repo: "r", Title: "Scenario blocked", State: "BLOCKED", BlockedReason: "needs operator"}, {Key: "orphaned", Repo: "r", Title: "Scenario orphaned", State: "ORPHANED"}}}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	// Model the lifecycle controller's ownership record for its external short socket.
	record, _ := json.Marshal(map[string]any{"stateDir": stack.Home(), "sockets": map[string]string{"bossd": stack.SocketPath()}})
	if err := os.WriteFile(filepath.Join(stack.Home(), "state.json"), record, 0o600); err != nil {
		t.Fatal(err)
	}
	result := scenario.Apply(ctx, doc, scenario.ApplyOptions{Home: stack.Home(), DB: filepath.Join(stack.Home(), "data", "bossd.db"), Socket: stack.SocketPath(), Phase: "prepare"})
	if !result.OK {
		t.Fatal(result.Problems)
	}
	repos, err := stack.Client().ListRepos(ctx, realstackrpc.Request(&pb.ListReposRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	for _, repo := range repos.Msg.Repos {
		if repo.Id == result.Keys.Repos["r"] && !strings.HasPrefix(repo.OriginUrl, "https://github.com/scenario/") {
			t.Fatalf("scenario repo cannot use GitHub inputs: %s", repo.OriginUrl)
		}
	}
	data, err := os.ReadFile(filepath.Join(stack.Home(), "scenario-state.json"))
	if err != nil {
		t.Fatal(err)
	}
	var st struct {
		Spec scenario.SeedSpec `json:"spec"`
	}
	if err = json.Unmarshal(data, &st); err != nil {
		t.Fatal(err)
	}
	spec, err := json.Marshal(st.Spec)
	if err != nil {
		t.Fatal(err)
	}
	ids, err := stack.Seed(spec)
	if err != nil {
		t.Fatal(err)
	}
	response, err := stack.Client().ListSessions(ctx, realstackrpc.Request(&pb.ListSessionsRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	for _, input := range doc.Sessions {
		var found *pb.Session
		for _, session := range response.Msg.Sessions {
			if session.Id == ids[input.Key] {
				found = session
				break
			}
		}
		if found == nil {
			t.Fatalf("missing served session %s", input.Key)
		}
		expected := displaystatus.Compute(displaystatus.Input{Session: &pb.Session{State: pb.SessionState(pb.SessionState_value["SESSION_STATE_"+input.State]), BlockedReason: &input.BlockedReason}})
		if found.DisplayIntent != expected.Intent {
			t.Fatalf("%s served intent %v, computed %v", input.Key, found.DisplayIntent, expected.Intent)
		}
	}
}
