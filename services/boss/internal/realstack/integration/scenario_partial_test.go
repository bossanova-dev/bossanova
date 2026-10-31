package integration_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/recurser/boss/internal/adapter/realstackrpc"
	"github.com/recurser/boss/internal/realstack"
	"github.com/recurser/boss/internal/scenario"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
)

// Run in full with: go test -race -cover ./internal/realstack/integration -run TestScenarioPartialBuildRealStack -count=1
func TestScenarioPartialBuildRealStack(t *testing.T) {
	if testing.Short() {
		t.Skip("run go test -race -cover ./internal/realstack/integration -run TestScenarioPartialBuildRealStack -count=1")
	}
	stack := realstack.Start(t, realstack.Options{})
	data, err := os.ReadFile("../../../../../docs/features/scenarios/chat-picker/partial-build-parked.json")
	if err != nil {
		t.Fatal(err)
	}
	doc, problems := scenario.Decode(data)
	if len(problems) > 0 {
		t.Fatal(problems)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
	defer cancel()
	record, _ := json.Marshal(map[string]any{"stateDir": stack.Home(), "sockets": map[string]string{"bossd": stack.SocketPath()}})
	if err = os.WriteFile(filepath.Join(stack.Home(), "state.json"), record, 0600); err != nil {
		t.Fatal(err)
	}
	opts := scenario.ApplyOptions{Home: stack.Home(), DB: filepath.Join(stack.Home(), "data", "bossd.db"), Socket: stack.SocketPath(), Settings: stack.SettingsPath(), Phase: "prepare"}
	result := scenario.Apply(ctx, doc, opts)
	if !result.OK {
		t.Fatal(result.Problems)
	}
	data, err = os.ReadFile(filepath.Join(stack.Home(), "scenario-state.json"))
	if err != nil {
		t.Fatal(err)
	}
	var prepared struct {
		Spec                     scenario.SeedSpec `json:"spec"`
		Origins, Branches, Heads map[string]string
	}
	if err = json.Unmarshal(data, &prepared); err != nil {
		t.Fatal(err)
	}
	plan, problems := scenario.Compile(doc)
	if len(problems) > 0 {
		t.Fatal(problems)
	}
	for i := range plan.GH {
		response := &plan.GH[i]
		for j, arg := range response.Args {
			if j > 0 && response.Args[j-1] == "--repo" {
				response.Args[j] = prepared.Origins[arg]
			}
		}
		for key, origin := range prepared.Origins {
			if response.Args[0] == "api" {
				response.Args[1] = strings.Replace(response.Args[1], "repos/"+key, "repos/"+origin, 1)
			}
		}
		for key, branch := range prepared.Branches {
			response.Stdout = strings.ReplaceAll(response.Stdout, `"headRefName":"`+key+`"`, `"headRefName":"`+branch+`"`)
		}
		response.Stdout = strings.ReplaceAll(response.Stdout, `"headRefOid":"branch-head"`, `"headRefOid":"`+prepared.Heads["partial"]+`"`)
	}
	stack.SetGHResponses(t, plan.GH)
	spec, _ := json.Marshal(prepared.Spec)
	ids := stack.SeedDB(t, spec)
	var state map[string]any
	if err = json.Unmarshal(data, &state); err != nil {
		t.Fatal(err)
	}
	state["keys"].(map[string]any)["sessions"] = ids
	updated, err := json.Marshal(state)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(stack.Home(), "scenario-state.json"), updated, 0600); err != nil {
		t.Fatal(err)
	}
	opts.Phase = "online"
	// SeedDB returned the same persisted keys the online phase consumes.
	result = scenario.Apply(ctx, doc, opts)
	if !result.OK {
		t.Fatal(result.Problems)
	}
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()
	for {
		response, err := stack.Client().GetSession(ctx, realstackrpc.Request(&pb.GetSessionRequest{Id: ids["partial"]}))
		if err != nil {
			t.Fatal(err)
		}
		row := response.Msg.Session
		a := row.GetAttention()
		if a.GetKind() == pb.AttentionKind_ATTENTION_KIND_FIX && a.GetSentence() == "Not verified · blocked · only part of the ticket was finished, so the PR is held" && slices.Contains(a.GetParkCodes(), "partial-build") {
			if slices.Contains(a.GetActions(), pb.AttentionAction_ATTENTION_ACTION_MERGE) {
				t.Fatal("partial build offered merge")
			}
			if prepared.Heads["partial"] == "" {
				t.Fatal("missing matching branch HEAD")
			}
			t.Logf("served partial-build FIX from matching-head PR %s", prepared.Heads["partial"])
			return
		}
		select {
		case <-ctx.Done():
			t.Fatalf("served partial-build attention never arrived: %v; %s", a, stack.LogTail())
		case <-ticker.C:
		}
	}
}
