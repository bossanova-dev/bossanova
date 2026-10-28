package scenario

import (
	"context"
	"errors"
	"os"
	"reflect"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/kit/safego"
	"github.com/rs/zerolog"
)

func TestCompileHeldMergeArgv(t *testing.T) {
	for _, strategy := range []string{"", "merge", "squash", "rebase"} {
		doc := Document{Version: 1, Name: "hold", Repos: []Repo{{Key: "r", DisplayName: "Repo", MergeStrategy: strategy}}, Sessions: []Session{{Key: "blocked", Repo: "r", Title: "Blocked", State: "BLOCKED", PR: &PullRequest{Number: 42, State: "OPEN", Checks: "PASSED", Mergeable: true}}}, Operations: []Operation{{Op: "holdMerge", Session: "blocked"}}}
		plan, problems := Compile(doc)
		if len(problems) > 0 {
			t.Fatal(problems)
		}
		flag := strategy
		if flag == "" {
			flag = "merge"
		}
		want := []string{"pr", "merge", "42", "--repo", "r", "--" + flag, "--delete-branch"}
		found := false
		for _, response := range plan.GH {
			if reflect.DeepEqual(response.Args, want) {
				found = true
				if response.Hold == "" {
					t.Fatal("merge response does not hold")
				}
			}
		}
		if !found {
			t.Fatalf("missing held merge argv %v", want)
		}
	}
}

type scenarioOperationDaemon struct {
	home     string
	mergeErr error
	deferred bool
	reports  []pb.ChatStatus
	mu       sync.Mutex
}

func (d *scenarioOperationDaemon) MergeSession(ctx context.Context, _ string) error {
	if d.mergeErr != nil {
		return d.mergeErr
	}
	name := operationName(Operation{Op: "holdMerge", Session: "s"})
	if err := os.WriteFile(marker(d.home, name, "entered"), []byte(`{}`), 0600); err != nil {
		return err
	}
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for !exists(marker(d.home, name, "release")) {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
	return nil
}
func (d *scenarioOperationDaemon) ArchiveSession(context.Context, string) (bool, error) {
	return d.deferred, nil
}
func (d *scenarioOperationDaemon) ReportChatStatus(_ context.Context, _ string, status pb.ChatStatus, _ time.Time) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.reports = append(d.reports, status)
	return nil
}

func (d *scenarioOperationDaemon) statuses() []pb.ChatStatus {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]pb.ChatStatus{}, d.reports...)
}

func TestOperationsEnterAndRelease(t *testing.T) {
	for _, op := range []string{"holdMerge", "holdArchive"} {
		t.Run(op, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				home := t.TempDir()
				client := &scenarioOperationDaemon{home: home, deferred: true}
				doc := Document{Sessions: []Session{{Key: "s", Chats: []Chat{{Key: "chat"}}}}, Operations: []Operation{{Op: op, Session: "s"}}}
				ready := make(chan struct{})
				var problems []Problem
				done := safego.Go(zerolog.Nop(), func() {
					problems = RunOperations(t.Context(), doc, Keys{Sessions: map[string]string{"s": "id"}}, home, client, func() error { close(ready); return nil })
				})
				<-ready
				synctest.Wait()
				if op == "holdMerge" && !exists(marker(home, operationName(doc.Operations[0]), "entered")) {
					t.Fatal("ready before fake gh entered")
				}
				if op == "holdArchive" && (len(client.statuses()) == 0 || client.statuses()[0] != pb.ChatStatus_CHAT_STATUS_WORKING) {
					t.Fatal("archive was not held busy")
				}
				if op == "holdArchive" {
					time.Sleep(30 * time.Second)
					synctest.Wait()
					if len(client.statuses()) < 3 {
						t.Fatal("archive keeper did not refresh its busy report")
					}
				}
				result := ReleaseOperations(t.Context(), home, nil)
				if !result.OK || len(result.Operations) != 1 {
					t.Fatalf("release: %+v", result)
				}
				<-done
				if len(problems) > 0 {
					t.Fatal(problems)
				}
				if result = ReleaseOperations(t.Context(), home, []string{"s"}); !result.OK {
					t.Fatal("release not idempotent", result)
				}
				if op == "holdArchive" && client.statuses()[len(client.statuses())-1] != pb.ChatStatus_CHAT_STATUS_IDLE {
					t.Fatal("archive busy heartbeat survived release")
				}
			})
		})
	}
}
func TestOperationsRefusal(t *testing.T) {
	for _, op := range []string{"holdMerge", "holdArchive"} {
		t.Run(op, func(t *testing.T) {
			client := &scenarioOperationDaemon{home: t.TempDir(), mergeErr: errors.New("merge blocked: gate=checks; failing checks")}
			doc := Document{Sessions: []Session{{Key: "s", Chats: []Chat{{Key: "c"}}}}, Operations: []Operation{{Op: op, Session: "s"}}}
			problems := RunOperations(t.Context(), doc, Keys{Sessions: map[string]string{"s": "id"}}, client.home, client, func() error { t.Fatal("refusal reported ready"); return nil })
			if len(problems) != 1 || problems[0].Code != "operation-refused" || problems[0].Hint == "" {
				t.Fatal(problems)
			}
			if op == "holdMerge" && !strings.Contains(problems[0].Message, "failing checks") {
				t.Fatal("daemon reason lost", problems)
			}
		})
	}
}
