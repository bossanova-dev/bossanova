package fixtures

import (
	"strings"
	"testing"
	"time"

	pb "github.com/recurser/bossalib/gen/bossanova/v1"
)

// fakeOverlayTarget records every Add* call so tests can assert ApplyOverlay
// mapped the overlay JSON onto the right pb objects. It satisfies the narrow
// overlayTarget interface without importing tuitest (import-cycle invariant).
type fakeOverlayTarget struct {
	repos    []*pb.Repo
	sessions []*pb.Session
	chats    []*pb.ClaudeChat
	crons    []*pb.CronJob
	prs      map[string][]*pb.PRSummary
	issues   map[string][]*pb.TrackerIssue
}

func newFakeTarget() *fakeOverlayTarget {
	return &fakeOverlayTarget{
		prs:    map[string][]*pb.PRSummary{},
		issues: map[string][]*pb.TrackerIssue{},
	}
}

func (f *fakeOverlayTarget) AddRepo(r *pb.Repo)       { f.repos = append(f.repos, r) }
func (f *fakeOverlayTarget) AddSession(s *pb.Session) { f.sessions = append(f.sessions, s) }
func (f *fakeOverlayTarget) AddChat(c *pb.ClaudeChat) { f.chats = append(f.chats, c) }
func (f *fakeOverlayTarget) AddCronJob(j *pb.CronJob) { f.crons = append(f.crons, j) }
func (f *fakeOverlayTarget) AddPRs(repoID string, prs []*pb.PRSummary) {
	f.prs[repoID] = append(f.prs[repoID], prs...)
}
func (f *fakeOverlayTarget) AddTrackerIssues(repoID string, issues []*pb.TrackerIssue) {
	f.issues[repoID] = append(f.issues[repoID], issues...)
}

func TestApplyOverlayMinimalSessionDefaults(t *testing.T) {
	o, err := ParseOverlay([]byte(`{
		"sessions": [
			{"id": "sess-x", "title": "Seeded session", "createdOffsetMins": -15}
		]
	}`))
	if err != nil {
		t.Fatalf("ParseOverlay: %v", err)
	}
	target := newFakeTarget()
	if err := ApplyOverlay(target, o); err != nil {
		t.Fatalf("ApplyOverlay: %v", err)
	}
	if len(target.sessions) != 1 {
		t.Fatalf("want 1 session, got %d", len(target.sessions))
	}
	s := target.sessions[0]
	if s.Id != "sess-x" || s.Title != "Seeded session" {
		t.Fatalf("bad session mapping: %+v", s)
	}
	// Offset-only timestamp maps through ts(): fixedNow + (-15m).
	wantTS := ts(-15 * time.Minute).AsTime()
	if !s.CreatedAt.AsTime().Equal(wantTS) {
		t.Fatalf("CreatedAt = %v, want %v (fixedNow -15m)", s.CreatedAt.AsTime(), wantTS)
	}
	// Absent state defaults to UNSPECIFIED (documented).
	if s.State != pb.SessionState_SESSION_STATE_UNSPECIFIED {
		t.Fatalf("absent state should default UNSPECIFIED, got %v", s.State)
	}
	if s.PrNumber != nil {
		t.Fatalf("absent prNumber should be nil, got %v", *s.PrNumber)
	}
}

func TestApplyOverlaySessionStateAndPR(t *testing.T) {
	// Accept both the short ("READY_FOR_REVIEW") and full form of the state name.
	for _, name := range []string{"READY_FOR_REVIEW", "SESSION_STATE_READY_FOR_REVIEW"} {
		o, err := ParseOverlay([]byte(`{"sessions":[{"id":"s1","title":"t","state":"` + name + `","prNumber":42}]}`))
		if err != nil {
			t.Fatalf("ParseOverlay(%s): %v", name, err)
		}
		target := newFakeTarget()
		if err := ApplyOverlay(target, o); err != nil {
			t.Fatalf("ApplyOverlay(%s): %v", name, err)
		}
		s := target.sessions[0]
		if s.State != pb.SessionState_SESSION_STATE_READY_FOR_REVIEW {
			t.Fatalf("state %s -> %v, want READY_FOR_REVIEW", name, s.State)
		}
		if s.PrNumber == nil || *s.PrNumber != 42 {
			t.Fatalf("prNumber not mapped to *int32(42): %+v", s.PrNumber)
		}
	}
}

func TestApplyOverlayBadStateNameListsValid(t *testing.T) {
	o, err := ParseOverlay([]byte(`{"sessions":[{"id":"s1","title":"t","state":"NONSENSE"}]}`))
	if err != nil {
		t.Fatalf("ParseOverlay: %v", err)
	}
	err = ApplyOverlay(newFakeTarget(), o)
	if err == nil {
		t.Fatal("expected error for bad state name")
	}
	// Error must name the offending value AND list a valid state.
	if !strings.Contains(err.Error(), "NONSENSE") {
		t.Fatalf("error should name the bad value: %v", err)
	}
	if !strings.Contains(err.Error(), "READY_FOR_REVIEW") {
		t.Fatalf("error should list valid states: %v", err)
	}
}

func TestApplyOverlayAllEntitiesWithDefaults(t *testing.T) {
	o, err := ParseOverlay([]byte(`{
		"repos": [{"id": "repo-x", "displayName": "widget-app"}],
		"sessions": [{"id": "sx", "title": "sess"}],
		"chats": [{"id": "cx", "sessionId": "sx", "title": "chat"}],
		"cronJobs": [{"id": "jx", "name": "nightly"}],
		"prs": [{"repoId": "repo-x", "prs": [{"number": 7, "title": "PR seven", "state": "OPEN"}]}],
		"trackerIssues": [{"repoId": "repo-x", "issues": [{"externalId": "ENG-1", "title": "Issue one"}]}]
	}`))
	if err != nil {
		t.Fatalf("ParseOverlay: %v", err)
	}
	target := newFakeTarget()
	if err := ApplyOverlay(target, o); err != nil {
		t.Fatalf("ApplyOverlay: %v", err)
	}
	if len(target.repos) != 1 || target.repos[0].DefaultBaseBranch != "main" || target.repos[0].MergeStrategy != "merge" {
		t.Fatalf("repo defaults wrong: %+v", target.repos)
	}
	if len(target.crons) != 1 {
		t.Fatalf("want 1 cron, got %d", len(target.crons))
	}
	j := target.crons[0]
	if j.Schedule != "@daily" || j.Timezone != "UTC" || j.AgentName != "claude" || !j.IsEnabled {
		t.Fatalf("cron defaults wrong (enabled should default true): %+v", j)
	}
	prs := target.prs["repo-x"]
	if len(prs) != 1 || prs[0].Number != 7 || prs[0].State != pb.PRState_PR_STATE_OPEN {
		t.Fatalf("pr mapping wrong: %+v", prs)
	}
	issues := target.issues["repo-x"]
	if len(issues) != 1 || issues[0].ExternalId != "ENG-1" {
		t.Fatalf("tracker issue mapping wrong: %+v", issues)
	}
}

func TestApplyOverlayCronEnabledExplicitFalse(t *testing.T) {
	o, err := ParseOverlay([]byte(`{"cronJobs":[{"id":"jx","name":"n","enabled":false}]}`))
	if err != nil {
		t.Fatalf("ParseOverlay: %v", err)
	}
	target := newFakeTarget()
	if err := ApplyOverlay(target, o); err != nil {
		t.Fatalf("ApplyOverlay: %v", err)
	}
	if target.crons[0].IsEnabled {
		t.Fatal("explicit enabled:false must stay false")
	}
}

func TestParseOverlayRejectsUnknownTopLevelField(t *testing.T) {
	_, err := ParseOverlay([]byte(`{"bogusTop": 1}`))
	if err == nil {
		t.Fatal("expected DisallowUnknownFields error for unknown top-level field")
	}
	if !strings.Contains(err.Error(), "bogusTop") {
		t.Fatalf("error should name the unknown field: %v", err)
	}
}

func TestParseOverlayRejectsUnknownEntityField(t *testing.T) {
	_, err := ParseOverlay([]byte(`{"sessions":[{"id":"s","title":"t","nopeField":1}]}`))
	if err == nil {
		t.Fatal("expected DisallowUnknownFields error for unknown entity field")
	}
	if !strings.Contains(err.Error(), "nopeField") {
		t.Fatalf("error should name the unknown field: %v", err)
	}
}

// TestParseOverlayRejectsTrailingJSON pins the dec.More() single-document guard:
// a second top-level value (or trailing garbage) after the overlay object is
// rejected loudly, while a lone object with trailing whitespace is accepted.
func TestParseOverlayRejectsTrailingJSON(t *testing.T) {
	reject := []string{
		`{} {}`,                        // two top-level objects
		`{"repos":[]} {"sessions":[]}`, // second overlay object
		`{"repos":[]} garbage`,         // trailing non-JSON
		"{}\n{}",                       // newline-separated second object
	}
	for _, in := range reject {
		if _, err := ParseOverlay([]byte(in)); err == nil {
			t.Fatalf("ParseOverlay(%q) should reject trailing JSON", in)
		} else if !strings.Contains(err.Error(), "trailing") {
			t.Fatalf("ParseOverlay(%q) error should mention trailing JSON: %v", in, err)
		}
	}
	// A single object with surrounding whitespace is still accepted.
	for _, in := range []string{`{}`, "  {\"repos\":[]}\n", "\t{}\t"} {
		if _, err := ParseOverlay([]byte(in)); err != nil {
			t.Fatalf("ParseOverlay(%q) should accept a lone object: %v", in, err)
		}
	}
}

func TestApplyOverlayRequiredFields(t *testing.T) {
	cases := map[string]string{
		"session missing id":    `{"sessions":[{"title":"t"}]}`,
		"session missing title": `{"sessions":[{"id":"s"}]}`,
		"repo missing display":  `{"repos":[{"id":"r"}]}`,
		"chat missing session":  `{"chats":[{"id":"c","title":"t"}]}`,
		"cron missing name":     `{"cronJobs":[{"id":"j"}]}`,
		"prs missing repoId":    `{"prs":[{"prs":[{"number":1,"title":"t"}]}]}`,
		"pr missing number":     `{"prs":[{"repoId":"r","prs":[{"title":"t"}]}]}`,
		"issue missing extId":   `{"trackerIssues":[{"repoId":"r","issues":[{"title":"t"}]}]}`,
	}
	for name, body := range cases {
		o, err := ParseOverlay([]byte(body))
		if err != nil {
			t.Fatalf("%s: ParseOverlay: %v", name, err)
		}
		if err := ApplyOverlay(newFakeTarget(), o); err == nil {
			t.Fatalf("%s: expected required-field error", name)
		}
	}
}
