package server

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/recurser/bossalib/attention"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/kit/vcs"
	"github.com/recurser/bossalib/machine"
	"github.com/recurser/bossalib/models"
	"github.com/recurser/bossd/internal/db"
	"github.com/recurser/bossd/internal/domain"
	"github.com/recurser/bossd/internal/status"
)

func TestAttentionVerifyAdmission(t *testing.T) {
	for _, tc := range []struct {
		name, state, head, reason, label string
		invalid                          string
		missing, partial                 bool
		want                             pb.AttentionKind
	}{
		{name: "excluded", state: "excluded", head: timelineHead, want: pb.AttentionKind_ATTENTION_KIND_MERGE},
		{name: "admitted", state: "admitted", head: timelineHead, label: "queued", want: pb.AttentionKind_ATTENTION_KIND_IN_PROGRESS},
		{name: "pending", state: "pending", head: timelineHead, reason: "ticket-not-in-review", label: "needs In Review", want: pb.AttentionKind_ATTENTION_KIND_IN_PROGRESS},
		{name: "pending unknown", state: "pending", head: timelineHead, reason: "unknown:tracker", label: "verify unknown", want: pb.AttentionKind_ATTENTION_KIND_IN_PROGRESS},
		{name: "old head", state: "admitted", head: strings.Repeat("1", 40), label: "verify unknown", want: pb.AttentionKind_ATTENTION_KIND_IN_PROGRESS},
		{name: "missing", missing: true, label: "verify unknown", want: pb.AttentionKind_ATTENTION_KIND_IN_PROGRESS},
		{name: "unreadable", invalid: "{", label: "verify unknown", want: pb.AttentionKind_ATTENTION_KIND_IN_PROGRESS},
		{name: "other version", invalid: `{"version":2}`, label: "verify unknown", want: pb.AttentionKind_ATTENTION_KIND_IN_PROGRESS},
		{name: "partial excluded", state: "excluded", head: timelineHead, partial: true, want: pb.AttentionKind_ATTENTION_KIND_FIX},
		{name: "partial admitted", state: "admitted", head: timelineHead, partial: true, want: pb.AttentionKind_ATTENTION_KIND_FIX},
		{name: "partial pending", state: "pending", head: timelineHead, reason: "ticket-not-in-review", partial: true, want: pb.AttentionKind_ATTENTION_KIND_FIX},
		{name: "partial missing", missing: true, partial: true, want: pb.AttentionKind_ATTENTION_KIND_FIX},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var data []byte
			if !tc.missing {
				data = admissionSnapshot(t, tc.head, tc.state, tc.reason)
				if tc.invalid != "" {
					data = []byte(tc.invalid)
				}
			}
			got, facts := boardAttentionWithAdmission(t, data, tc.partial)
			if (facts.VerifyAdmission.State == attention.AdmissionExcluded) != (tc.state == "excluded" && tc.head == timelineHead) {
				t.Fatalf("timeline admission=%v disagrees with board attention=%v", facts.VerifyAdmission, tc.want)
			}
			if got.GetKind() != tc.want {
				t.Fatalf("attention = %v, want %v", got, tc.want)
			}
			if tc.partial {
				if got.GetSentence() != "Partial build" {
					t.Fatalf("partial attention = %v", got)
				}
			} else if tc.want == pb.AttentionKind_ATTENTION_KIND_MERGE {
				if !got.GetIsUnverified() || got.GetProgressLabel() == "queued" {
					t.Fatalf("excluded attention = %v", got)
				}
			} else if got.GetProgressLabel() != tc.label || attention.NeedsYou(got.GetKind()) {
				t.Fatalf("factory attention = %v", got)
			}
		})
	}
}

// admissionSnapshot is a version-one admission file answering PR 1 at head.
func admissionSnapshot(t *testing.T, head, state, reason string) []byte {
	t.Helper()
	snapshot := map[string]any{"version": 1, "scopes": map[string]any{"default": map[string]any{"prs": map[string]any{"1": map[string]string{"head": head, "state": state, "reason": reason}}}}}
	data, err := json.Marshal(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

// boardAttentionWithAdmission lists a green, mergeable PR session in a repository with an enabled
// verify cron whose admission file holds data (none when nil), returning its board attention and
// its timeline facts.
func boardAttentionWithAdmission(t *testing.T, data []byte, partial bool) (*pb.SessionAttention, timelineFacts) {
	t.Helper()
	repoPath := t.TempDir()
	cmd := exec.CommandContext(t.Context(), "git", "init", repoPath)
	cmd.WaitDelay = time.Second
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git init: %s: %v", out, err)
	}
	if data != nil {
		dir := filepath.Join(repoPath, ".git", "boss-verify")
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "verify-admission.json"), data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	n, mergeable := 1, true
	row := &models.Session{ID: "s", RepoID: "r", PRNumber: &n, State: machine.ReadyForReview}
	tr := status.NewDisplayTracker()
	tr.Set("s", vcs.DisplayInfo{Status: vcs.DisplayStatusPassing, HeadSHA: timelineHead, Mergeable: &mergeable, IsPartial: partial})
	s := newListSessionsDisplayStatusTestServer([]*models.Session{row}, nil, tr, nil)
	s.repos = &listSessionsRepoStoreFake{repos: map[string]*models.Repo{"r": {ID: "r", LocalPath: repoPath}}}
	s.cronJobs = &attentionCronStore{jobs: []*domain.CronJob{{Prompt: "/boss-verify", IsEnabled: true}}}
	s.checkSnapshots = &fakeCheckSnapshotStore{snaps: []db.CheckSnapshot{{HeadSHA: timelineHead, RawJSON: checksJSON(t, greenCI(2, ""))}}}
	resp, err := s.ListSessions(t.Context(), connect.NewRequest(&pb.ListSessionsRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	facts, err := s.timelineFacts(t.Context(), row)
	if err != nil {
		t.Fatal(err)
	}
	return onlySession(t, resp.Msg.Sessions).GetAttention(), facts
}

// The admission policy's reasons ride on the existing states: a policy-admitted ticketless PR
// reads queued, and an opted-out or unadmitted one keeps the unverified merge path.
func TestAttentionVerifyAdmissionPolicyReasons_Sessions_AC121(t *testing.T) {
	for _, tc := range []struct{ state, reason string }{
		{"admitted", "policy:human"},
		{"admitted", "policy:bot"},
		{"excluded", "opted-out"},
		{"excluded", "not-admitted:human"},
		{"excluded", "not-admitted:fork"},
	} {
		t.Run(tc.reason, func(t *testing.T) {
			got, _ := boardAttentionWithAdmission(t, admissionSnapshot(t, timelineHead, tc.state, tc.reason), false)
			if tc.state == "admitted" {
				if got.GetKind() != pb.AttentionKind_ATTENTION_KIND_IN_PROGRESS || got.GetProgressLabel() != "queued" {
					t.Fatalf("admitted %s: attention = %v, want queued", tc.reason, got)
				}
				return
			}
			if got.GetKind() != pb.AttentionKind_ATTENTION_KIND_MERGE || !got.GetIsUnverified() || got.GetProgressLabel() == "queued" {
				t.Fatalf("excluded %s: attention = %v, want an unverified merge", tc.reason, got)
			}
		})
	}
}
