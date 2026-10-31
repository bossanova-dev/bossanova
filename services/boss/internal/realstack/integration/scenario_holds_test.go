package integration_test

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/recurser/boss/internal/adapter/realstackrpc"
	"github.com/recurser/boss/internal/realstack"
	"github.com/recurser/boss/internal/scenario"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/kit/safego"
	"github.com/rs/zerolog"
)

// TestMain keeps the cached realstack binaries alive for the entire package.
func TestMain(m *testing.M) {
	flag.Parse()
	if testing.Short() {
		os.Exit(m.Run())
	}
	cleanup, err := realstack.Build()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	code := m.Run()
	cleanup()
	os.Exit(code)
}

// Run in full with: go test -race -cover -coverpkg=./internal/scenario,./internal/realstack ./internal/realstack/integration -run TestScenarioHoldsRealStack -count=1
func TestScenarioHoldsRealStack(t *testing.T) {
	if testing.Short() {
		t.Skip("real daemon scenario holds; run go test -race -cover -coverpkg=./internal/scenario,./internal/realstack ./internal/realstack/integration -run TestScenarioHoldsRealStack")
	}
	stack := realstack.Start(t, realstack.Options{})
	_, source, _, _ := runtime.Caller(0)
	data, err := os.ReadFile(filepath.Join(filepath.Dir(source), "../../../../../docs/features/scenarios/chat-picker/errored-transient-intents.json"))
	if err != nil {
		t.Fatal(err)
	}
	doc, problems := scenario.Decode(data)
	if len(problems) > 0 {
		t.Fatal(problems)
	}
	var previous map[string][2]bool
	for i := 0; i < 2; i++ {
		served := exerciseScenarioHolds(t, stack, doc)
		if previous != nil && !reflect.DeepEqual(previous, served) {
			t.Fatalf("holds changed across applies: %v -> %v", previous, served)
		}
		previous = served
	}
}
func exerciseScenarioHolds(t *testing.T, stack *realstack.Stack, doc scenario.Document) map[string][2]bool {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 90*time.Second)
	defer cancel()
	opts := scenario.ApplyOptions{Home: stack.Home(), DB: filepath.Join(stack.Home(), "data", "bossd.db"), Socket: stack.SocketPath(), Settings: stack.SettingsPath(), Phase: "prepare"}
	record, _ := json.Marshal(map[string]any{"stateDir": stack.Home(), "sockets": map[string]string{"bossd": stack.SocketPath()}})
	if err := os.WriteFile(filepath.Join(stack.Home(), "state.json"), record, 0600); err != nil {
		t.Fatal(err)
	}
	result := scenario.Apply(ctx, doc, opts)
	if !result.OK {
		t.Fatal(result.Problems)
	}
	// realstack owns stop/seed/restart; consume exactly the prepare phase's input-only spec.
	data, err := os.ReadFile(filepath.Join(stack.Home(), "scenario-state.json"))
	if err != nil {
		t.Fatal(err)
	}
	var prepared struct {
		Spec     scenario.SeedSpec `json:"spec"`
		Origins  map[string]string `json:"origins"`
		Branches map[string]string `json:"branches"`
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
		if len(response.Args) > 1 && response.Args[0] == "api" {
			for key, origin := range prepared.Origins {
				response.Args[1] = strings.Replace(response.Args[1], "repos/"+key, "repos/"+origin, 1)
			}
		}
		for key, branch := range prepared.Branches {
			response.Stdout = strings.ReplaceAll(response.Stdout, fmt.Sprintf(`"headRefName":%q`, key), fmt.Sprintf(`"headRefName":%q`, branch))
		}
	}
	stack.SetGHResponses(t, plan.GH)
	spec, _ := json.Marshal(prepared.Spec)
	keys := result.Keys
	keys.Sessions = stack.SeedDB(t, spec)
	ready := make(chan struct{})
	var heldProblems []scenario.Problem
	done := safego.Go(zerolog.Nop(), func() {
		heldProblems = scenario.RunOperations(ctx, doc, keys, stack.Home(), realstackrpc.Operations{Client: stack.Client()}, func() error { close(ready); return nil })
	})
	defer func() { cancel(); <-done }()
	select {
	case <-ready:
	case <-done:
		t.Fatalf("operations refused: %+v\n%s", heldProblems, stack.LogTail())
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	served := map[string][2]bool{}
	for _, key := range []string{"blocked-merging", "orphaned-merging", "blocked-archiving", "orphaned-archiving"} {
		response, e := stack.Client().GetSession(ctx, realstackrpc.Request(&pb.GetSessionRequest{Id: keys.Sessions[key]}))
		if e != nil {
			t.Fatal(e)
		}
		served[key] = [2]bool{response.Msg.Session.DisplayMerging, response.Msg.Session.ArchivePending}
	}
	want := map[string][2]bool{"blocked-merging": {true, false}, "orphaned-merging": {true, false}, "blocked-archiving": {false, true}, "orphaned-archiving": {false, true}}
	if !reflect.DeepEqual(served, want) {
		t.Fatalf("served held inputs: %v want %v", served, want)
	}
	// The merged fixture may already be auto-archived when the daemon polls it.
	listed, err := stack.Client().ListSessions(ctx, realstackrpc.Request(&pb.ListSessionsRequest{IncludeArchived: true}))
	if err != nil {
		t.Fatal(err)
	}
	intents := map[string]pb.DisplayIntent{}
	for _, row := range listed.Msg.Sessions {
		for key, id := range keys.Sessions {
			if row.Id == id {
				intents[key] = row.DisplayIntent
			}
		}
	}
	wantIntents := map[string]pb.DisplayIntent{
		"blocked-merging":    pb.DisplayIntent_DISPLAY_INTENT_INFO,
		"orphaned-merging":   pb.DisplayIntent_DISPLAY_INTENT_INFO,
		"blocked-archiving":  pb.DisplayIntent_DISPLAY_INTENT_WARNING,
		"orphaned-archiving": pb.DisplayIntent_DISPLAY_INTENT_WARNING,
		"orphaned-passing":   pb.DisplayIntent_DISPLAY_INTENT_DANGER,
		"merged":             pb.DisplayIntent_DISPLAY_INTENT_MUTED,
		"closed":             pb.DisplayIntent_DISPLAY_INTENT_MUTED,
	}
	if !reflect.DeepEqual(intents, wantIntents) {
		t.Fatalf("ListSessions display intents: %v want %v", intents, wantIntents)
	}
	if release := scenario.ReleaseOperations(ctx, stack.Home(), nil); !release.OK {
		t.Fatal(release.Problems)
	}
	<-done
	if len(heldProblems) > 0 {
		t.Fatal(heldProblems)
	}
	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()
	for {
		settled := true
		for key, id := range keys.Sessions {
			response, e := stack.Client().GetSession(ctx, realstackrpc.Request(&pb.GetSessionRequest{Id: id}))
			if e != nil {
				t.Fatal(e)
			}
			row := response.Msg.Session
			if row.DisplayMerging || row.ArchivePending || (served[key][1] && row.ArchivedAt == nil) {
				settled = false
			}
		}
		if settled {
			break
		}
		select {
		case <-ctx.Done():
			t.Fatal("release did not settle", ctx.Err())
		case <-ticker.C:
		}
	}
	t.Logf("held inputs reproducible: %v; released merges and completed deferred archive", served)
	return served
}
