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

// TestApplyOverlaySessionHTTPPorts pins the BOS-474 overlay field: httpPorts
// becomes one loopback HttpEndpoint per port, in the order given.
func TestApplyOverlaySessionHTTPPorts(t *testing.T) {
	o, err := ParseOverlay([]byte(`{"sessions":[{"id":"s1","title":"t","httpPorts":[3000,5173]}]}`))
	if err != nil {
		t.Fatalf("ParseOverlay: %v", err)
	}
	target := newFakeTarget()
	if err := ApplyOverlay(target, o); err != nil {
		t.Fatalf("ApplyOverlay: %v", err)
	}
	eps := target.sessions[0].GetHttpEndpoints()
	if len(eps) != 2 {
		t.Fatalf("http endpoints = %d, want 2: %+v", len(eps), eps)
	}
	for i, want := range []struct {
		port uint32
		url  string
	}{{3000, "http://127.0.0.1:3000"}, {5173, "http://127.0.0.1:5173"}} {
		if eps[i].GetPort() != want.port || eps[i].GetUrl() != want.url {
			t.Errorf("endpoint %d = {%d, %q}, want {%d, %q}", i, eps[i].GetPort(), eps[i].GetUrl(), want.port, want.url)
		}
	}
}

// TestApplyOverlaySessionHTTPPortsRejectsOutOfRange keeps a bad port from
// silently wrapping into a nonsense uint32.
func TestApplyOverlaySessionHTTPPortsRejectsOutOfRange(t *testing.T) {
	for _, bad := range []string{"0", "-1", "65536"} {
		o, err := ParseOverlay([]byte(`{"sessions":[{"id":"s1","title":"t","httpPorts":[` + bad + `]}]}`))
		if err != nil {
			t.Fatalf("ParseOverlay(%s): %v", bad, err)
		}
		if err := ApplyOverlay(newFakeTarget(), o); err == nil {
			t.Errorf("ApplyOverlay with httpPorts [%s]: want an error, got nil", bad)
		}
	}
}

// TestApplyOverlaySessionRotationEvents pins the BOS-506 overlay field: a
// rotationEvents entry becomes one pb.RotationEvent on the owning session,
// carrying the session's own id so the row is self-consistent.
func TestApplyOverlaySessionRotationEvents(t *testing.T) {
	o, err := ParseOverlay([]byte(`{"sessions":[{"id":"s1","title":"t","rotationEvents":[{"id":"rot-1","outcome":"UNSPECIFIED","detail":"pane recovered before rotation"}]}]}`))
	if err != nil {
		t.Fatalf("ParseOverlay: %v", err)
	}
	target := newFakeTarget()
	if err := ApplyOverlay(target, o); err != nil {
		t.Fatalf("ApplyOverlay: %v", err)
	}
	evs := target.sessions[0].GetRotationEvents()
	if len(evs) != 1 {
		t.Fatalf("rotation events = %d, want 1: %+v", len(evs), evs)
	}
	ev := evs[0]
	if ev.GetId() != "rot-1" {
		t.Errorf("id = %q, want %q", ev.GetId(), "rot-1")
	}
	if ev.GetSessionId() != "s1" {
		t.Errorf("sessionId = %q, want the owning session %q", ev.GetSessionId(), "s1")
	}
	if ev.GetOutcome() != pb.RotationOutcome_ROTATION_OUTCOME_UNSPECIFIED {
		t.Errorf("outcome = %v, want UNSPECIFIED", ev.GetOutcome())
	}
	if ev.GetDetail() != "pane recovered before rotation" {
		t.Errorf("detail = %q, want the seeded detail", ev.GetDetail())
	}
	// Absent createdOffsetMins pins to fixedNow exactly.
	if got, want := ev.GetCreatedAt().AsTime(), ts(0).AsTime(); !got.Equal(want) {
		t.Errorf("CreatedAt = %v, want %v (fixedNow)", got, want)
	}
}

// TestApplyOverlaySessionRotationEventRotated covers the display-relevant
// ROTATED shape (views.rotationEventLabel renders "<from> switched to <to>"),
// accepting both the short and full enum names.
func TestApplyOverlaySessionRotationEventRotated(t *testing.T) {
	for _, name := range []string{"ROTATED", "ROTATION_OUTCOME_ROTATED"} {
		o, err := ParseOverlay([]byte(`{"sessions":[{"id":"s1","title":"t","rotationEvents":[{"id":"rot-1","outcome":"` + name + `","fromAccount":"personal","toAccount":"work"}]}]}`))
		if err != nil {
			t.Fatalf("ParseOverlay(%s): %v", name, err)
		}
		target := newFakeTarget()
		if err := ApplyOverlay(target, o); err != nil {
			t.Fatalf("ApplyOverlay(%s): %v", name, err)
		}
		ev := target.sessions[0].GetRotationEvents()[0]
		if ev.GetOutcome() != pb.RotationOutcome_ROTATION_OUTCOME_ROTATED {
			t.Errorf("outcome %s -> %v, want ROTATED", name, ev.GetOutcome())
		}
		if ev.GetFromAccount() != "personal" || ev.GetToAccount() != "work" {
			t.Errorf("accounts = (%q, %q), want (personal, work)", ev.GetFromAccount(), ev.GetToAccount())
		}
	}
}

// TestApplyOverlayBadRotationOutcomeListsValid keeps a typo'd outcome loud and
// self-explanatory, mirroring the state-name contract.
func TestApplyOverlayBadRotationOutcomeListsValid(t *testing.T) {
	o, err := ParseOverlay([]byte(`{"sessions":[{"id":"s1","title":"t","rotationEvents":[{"id":"rot-1","outcome":"NOPE"}]}]}`))
	if err != nil {
		t.Fatalf("ParseOverlay: %v", err)
	}
	err = ApplyOverlay(newFakeTarget(), o)
	if err == nil {
		t.Fatal("expected error for bad rotation outcome")
	}
	if !strings.Contains(err.Error(), "NOPE") {
		t.Errorf("error should name the bad value: %v", err)
	}
	if !strings.Contains(err.Error(), "ROTATED") {
		t.Errorf("error should list valid outcomes: %v", err)
	}
}

// TestApplyOverlayRotationEventRequiresID pins the required-field contract.
func TestApplyOverlayRotationEventRequiresID(t *testing.T) {
	o, err := ParseOverlay([]byte(`{"sessions":[{"id":"s1","title":"t","rotationEvents":[{"detail":"no id"}]}]}`))
	if err != nil {
		t.Fatalf("ParseOverlay: %v", err)
	}
	if err := ApplyOverlay(newFakeTarget(), o); err == nil {
		t.Fatal("expected required-field error for a rotation event with no id")
	}
}

// TestApplyOverlayRotationEventCreatedOffset pins the offset-only clock: no
// absolute timestamps in overlays.
func TestApplyOverlayRotationEventCreatedOffset(t *testing.T) {
	o, err := ParseOverlay([]byte(`{"sessions":[{"id":"s1","title":"t","rotationEvents":[{"id":"rot-1","createdOffsetMins":-7}]}]}`))
	if err != nil {
		t.Fatalf("ParseOverlay: %v", err)
	}
	target := newFakeTarget()
	if err := ApplyOverlay(target, o); err != nil {
		t.Fatalf("ApplyOverlay: %v", err)
	}
	got := target.sessions[0].GetRotationEvents()[0].GetCreatedAt().AsTime()
	if want := ts(-7 * time.Minute).AsTime(); !got.Equal(want) {
		t.Fatalf("CreatedAt = %v, want %v (fixedNow -7m)", got, want)
	}
	if got.Equal(ts(0).AsTime()) {
		t.Fatal("createdOffsetMins:-7 must differ from the zero-offset case")
	}
}
