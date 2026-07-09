package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"charm.land/bubbles/v2/spinner"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
)

// fakeDaemon is a programmable daemonMutator for protocol tests. The zero value
// is a valid no-op mutator (non-daemon tests pass &fakeDaemon{}).
type fakeDaemon struct {
	repos    []*pb.Repo
	sessions []*pb.Session
	chats    []*pb.ClaudeChat
	crons    []*pb.CronJob
	prs      map[string][]*pb.PRSummary
	issues   map[string][]*pb.TrackerIssue

	// attached reports SessionAttached; nil ⇒ every session reads as not attached.
	attached map[string]bool
	// pushErr, when set, is returned by every TryPush* — simulating a full/undrained
	// channel so a pusher errors without hanging the loop.
	pushErr error

	outputs      []pushedOutput
	stateChanges []pushedStateChange
	ended        []pushedEnded
}

type pushedOutput struct {
	sessionID, text string
}
type pushedStateChange struct {
	sessionID  string
	prev, next pb.SessionState
}
type pushedEnded struct {
	sessionID string
	final     pb.SessionState
}

func (d *fakeDaemon) AddRepo(r *pb.Repo)       { d.repos = append(d.repos, r) }
func (d *fakeDaemon) AddSession(s *pb.Session) { d.sessions = append(d.sessions, s) }
func (d *fakeDaemon) AddChat(c *pb.ClaudeChat) { d.chats = append(d.chats, c) }
func (d *fakeDaemon) AddCronJob(j *pb.CronJob) { d.crons = append(d.crons, j) }
func (d *fakeDaemon) AddPRs(repoID string, prs []*pb.PRSummary) {
	if d.prs == nil {
		d.prs = map[string][]*pb.PRSummary{}
	}
	d.prs[repoID] = append(d.prs[repoID], prs...)
}
func (d *fakeDaemon) AddTrackerIssues(repoID string, issues []*pb.TrackerIssue) {
	if d.issues == nil {
		d.issues = map[string][]*pb.TrackerIssue{}
	}
	d.issues[repoID] = append(d.issues[repoID], issues...)
}
func (d *fakeDaemon) SessionAttached(id string) bool { return d.attached[id] }
func (d *fakeDaemon) TryPushOutputLine(id, text string, _ time.Duration) error {
	if d.pushErr != nil {
		return d.pushErr
	}
	d.outputs = append(d.outputs, pushedOutput{id, text})
	return nil
}
func (d *fakeDaemon) TryPushStateChange(id string, prev, next pb.SessionState, _ time.Duration) error {
	if d.pushErr != nil {
		return d.pushErr
	}
	d.stateChanges = append(d.stateChanges, pushedStateChange{id, prev, next})
	return nil
}
func (d *fakeDaemon) TryPushSessionEnded(id string, final pb.SessionState, _ time.Duration) error {
	if d.pushErr != nil {
		return d.pushErr
	}
	d.ended = append(d.ended, pushedEnded{id, final})
	return nil
}

// fakeTUI is a programmable tui for protocol tests.
type fakeTUI struct {
	mu sync.Mutex

	// screens, when non-empty, are returned in sequence by Screen(); the last
	// value repeats once exhausted. When empty, screen is returned.
	screens []string
	idx     int
	screen  string

	waitErr error // returned by WaitForText
}

func (f *fakeTUI) Screen() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.screens) > 0 {
		if f.idx >= len(f.screens) {
			return f.screens[len(f.screens)-1]
		}
		s := f.screens[f.idx]
		f.idx++
		return s
	}
	return f.screen
}

func (f *fakeTUI) SendString(string) error  { return nil }
func (f *fakeTUI) PasteString(string) error { return nil }
func (f *fakeTUI) WaitForText(time.Duration, string) error {
	return f.waitErr
}

// fastSettle settles quickly for tests.
func fastSettle() settleConfig {
	return settleConfig{
		poll:      time.Millisecond,
		stableFor: 5 * time.Millisecond,
		hardCap:   500 * time.Millisecond,
		cols:      80,
		rows:      24,
	}
}

func decodeResponses(t *testing.T, out string) []map[string]any {
	t.Helper()
	var resps []map[string]any
	sc := bufio.NewScanner(strings.NewReader(out))
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			t.Fatalf("response line not JSON: %q: %v", line, err)
		}
		resps = append(resps, m)
	}
	return resps
}

// TestServeMalformedThenValid asserts a malformed line yields an error response
// and the loop keeps processing a following valid observe.
func TestServeMalformedThenValid(t *testing.T) {
	f := &fakeTUI{screen: "ready"}
	in := strings.NewReader("{not json\n" + `{"id":2,"op":"observe"}` + "\n")
	var out strings.Builder
	if err := serve(f, &fakeDaemon{}, in, &out, fastSettle()); err != nil {
		t.Fatalf("serve: %v", err)
	}
	resps := decodeResponses(t, out.String())
	if len(resps) != 2 {
		t.Fatalf("want 2 responses, got %d: %s", len(resps), out.String())
	}
	if ok, _ := resps[0]["ok"].(bool); ok {
		t.Fatalf("first response should be error, got %v", resps[0])
	}
	if resps[0]["error"] == nil {
		t.Fatalf("first response should carry an error message, got %v", resps[0])
	}
	if resps[1]["screen"] != "ready" {
		t.Fatalf("observe screen = %v, want ready", resps[1]["screen"])
	}
	if got := resps[1]["id"]; got != float64(2) {
		t.Fatalf("observe id = %v, want 2", got)
	}
}

// TestServeWaitTimeout asserts wait against an anchor the fake never shows
// returns ok:false / error:timeout within the timeout.
func TestServeWaitTimeout(t *testing.T) {
	f := &fakeTUI{screen: "loading", waitErr: errors.New("timeout after 30ms")}
	in := strings.NewReader(`{"id":7,"op":"wait","text":"never","timeoutMs":30}` + "\n")
	var out strings.Builder
	start := time.Now()
	if err := serve(f, &fakeDaemon{}, in, &out, fastSettle()); err != nil {
		t.Fatalf("serve: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("wait took too long: %v", elapsed)
	}
	resps := decodeResponses(t, out.String())
	if len(resps) != 1 {
		t.Fatalf("want 1 response, got %d", len(resps))
	}
	if ok, _ := resps[0]["ok"].(bool); ok {
		t.Fatalf("wait should fail, got %v", resps[0])
	}
	if resps[0]["error"] != "timeout" {
		t.Fatalf("wait error = %v, want timeout", resps[0]["error"])
	}
}

// TestSettleReturnsStableScreen asserts settle returns only after Screen()
// stops changing.
func TestSettleReturnsStableScreen(t *testing.T) {
	f := &fakeTUI{screens: []string{"loading", "loading", "ready"}}
	got := settle(f, fastSettle())
	if got != "ready" {
		t.Fatalf("settle = %q, want ready", got)
	}
}

// TestSettleFastOnSpinnerOnlyDiff: a screen whose only change is the spinner
// glyph must settle in ~stableFor (not hit hardCap) and return a RAW frame
// (still containing a real spinner glyph, not the placeholder).
func TestSettleFastOnSpinnerOnlyDiff(t *testing.T) {
	frames := spinner.Dot.Frames
	// Alternate different real glyph frames over identical content forever.
	f := &fakeTUI{screens: []string{
		frames[0] + "working  home",
		frames[1] + "working  home",
		frames[2] + "working  home",
		frames[3] + "working  home",
	}}
	st := fastSettle() // stableFor 5ms, hardCap 500ms
	start := time.Now()
	got := settle(f, st)
	if elapsed := time.Since(start); elapsed >= st.hardCap {
		t.Fatalf("settle hit hard cap on spinner-only diff: %v", elapsed)
	}
	// Returned frame is RAW: it must still contain a real braille glyph.
	if !strings.ContainsAny(got, "⣾⣽⣻⢿⡿⣟⣯⣷") {
		t.Fatalf("settle returned a normalized frame, want raw: %q", got)
	}
	if !strings.Contains(got, "working  home") {
		t.Fatalf("settle lost content: %q", got)
	}
}

// TestSettleWaitsOnRealContentChange: genuine content changes still reset the
// stability window, so settle returns only the final settled content.
func TestSettleWaitsOnRealContentChange(t *testing.T) {
	frames := spinner.Dot.Frames
	f := &fakeTUI{screens: []string{
		frames[0] + "loading",
		frames[1] + "loading",
		frames[2] + "ready", // real content change
	}}
	got := settle(f, fastSettle())
	if !strings.Contains(got, "ready") {
		t.Fatalf("settle = %q, want settled content 'ready'", got)
	}
}

func TestResolveSettleDefaultsWhenZero(t *testing.T) {
	base := fastSettle()
	got := resolveSettle(base, request{}) // no override
	if got.stableFor != base.stableFor || got.hardCap != base.hardCap {
		t.Fatalf("zero override changed timing: %+v vs base %+v", got, base)
	}
	if got.poll != base.poll || got.cols != base.cols || got.rows != base.rows {
		t.Fatalf("resolveSettle must preserve poll/cols/rows: %+v", got)
	}
}

func TestResolveSettleHonorsPositiveValues(t *testing.T) {
	base := fastSettle()
	got := resolveSettle(base, request{SettleMs: 120, HardCapMs: 900})
	if got.stableFor != 120*time.Millisecond {
		t.Fatalf("stableFor = %v, want 120ms", got.stableFor)
	}
	if got.hardCap != 900*time.Millisecond {
		t.Fatalf("hardCap = %v, want 900ms", got.hardCap)
	}
}

func TestResolveSettleClampsInsaneValues(t *testing.T) {
	base := fastSettle()
	// Absurdly large settle -> clamped to max; negative hardCap -> ignored, but
	// the "hardCap never below stableFor" invariant then raises hardCap to the
	// (clamped) stableFor since maxSettleMs exceeds base.hardCap.
	got := resolveSettle(base, request{SettleMs: 1 << 30, HardCapMs: -5})
	if got.stableFor != maxSettleMs*time.Millisecond {
		t.Fatalf("stableFor not clamped to max: %v", got.stableFor)
	}
	// hardCap must never be below stableFor.
	if got.hardCap < got.stableFor {
		t.Fatalf("hardCap %v < stableFor %v", got.hardCap, got.stableFor)
	}
	// With a small settle that stays under base.hardCap, a negative hardCap is
	// ignored and the base hardCap is preserved untouched.
	got2 := resolveSettle(base, request{SettleMs: 30, HardCapMs: -5})
	if got2.hardCap != base.hardCap {
		t.Fatalf("negative hardCap should keep base, got %v", got2.hardCap)
	}
}

// TestResolveSettleClampsPositiveBelowFloor exercises clampMs's lower bound: a
// positive override below the floor is raised UP to the floor (not dropped to
// base, which is the non-positive path). HardCapMs:50 is positive-below-floor,
// so it clamps to minHardCapMs (100ms) rather than keeping fastSettle's base;
// the "hardCap never below stableFor" invariant (100 >= 20) then holds trivially.
func TestResolveSettleClampsPositiveBelowFloor(t *testing.T) {
	base := fastSettle()
	got := resolveSettle(base, request{SettleMs: 5, HardCapMs: 50})
	if got.stableFor != minSettleMs*time.Millisecond {
		t.Fatalf("stableFor = %v, want floor %dms", got.stableFor, minSettleMs)
	}
	if got.hardCap != minHardCapMs*time.Millisecond {
		t.Fatalf("hardCap = %v, want floor %dms", got.hardCap, minHardCapMs)
	}
	if got.hardCap < got.stableFor {
		t.Fatalf("hardCap %v < stableFor %v", got.hardCap, got.stableFor)
	}
}

// TestServeObserveWithSettleOverride: an observe carrying settleMs/hardCapMs is
// accepted and still returns the screen (garbage/zero handled by resolveSettle,
// never crashes).
func TestServeObserveWithSettleOverride(t *testing.T) {
	f := &fakeTUI{screen: "ready"}
	in := strings.NewReader(`{"id":5,"op":"observe","settleMs":10,"hardCapMs":50}` + "\n")
	var out strings.Builder
	if err := serve(f, &fakeDaemon{}, in, &out, fastSettle()); err != nil {
		t.Fatalf("serve: %v", err)
	}
	resps := decodeResponses(t, out.String())
	if len(resps) != 1 || resps[0]["screen"] != "ready" {
		t.Fatalf("observe with override = %v", resps)
	}
}

// TestServeQuit asserts quit returns the quit sentinel and acks.
func TestServeQuit(t *testing.T) {
	f := &fakeTUI{screen: "ready"}
	in := strings.NewReader(`{"id":9,"op":"quit"}` + "\n")
	var out strings.Builder
	err := serve(f, &fakeDaemon{}, in, &out, fastSettle())
	if err != errQuit {
		t.Fatalf("serve returned %v, want errQuit", err)
	}
	resps := decodeResponses(t, out.String())
	if len(resps) != 1 {
		t.Fatalf("want 1 response, got %d", len(resps))
	}
	if ok, _ := resps[0]["ok"].(bool); !ok {
		t.Fatalf("quit ack should be ok:true, got %v", resps[0])
	}
	if got := resps[0]["id"]; got != float64(9) {
		t.Fatalf("quit id = %v, want 9", got)
	}
}

// TestServeUnknownOp asserts an unknown op yields an error response and keeps
// serving.
func TestServeUnknownOp(t *testing.T) {
	f := &fakeTUI{screen: "ready"}
	in := strings.NewReader(`{"id":3,"op":"frobnicate"}` + "\n")
	var out strings.Builder
	if err := serve(f, &fakeDaemon{}, in, &out, fastSettle()); err != nil {
		t.Fatalf("serve: %v", err)
	}
	resps := decodeResponses(t, out.String())
	if len(resps) != 1 {
		t.Fatalf("want 1 response, got %d", len(resps))
	}
	if ok, _ := resps[0]["ok"].(bool); ok {
		t.Fatalf("unknown op should fail, got %v", resps[0])
	}
	if msg, _ := resps[0]["error"].(string); !strings.Contains(msg, "unknown op") {
		t.Fatalf("unknown op error = %v", resps[0]["error"])
	}
}

// --- BOS-217 daemon + capabilities op tests ---

// runDaemonReqs feeds the given NDJSON request lines through serve against d and
// returns the decoded responses.
func runDaemonReqs(t *testing.T, d *fakeDaemon, lines ...string) []map[string]any {
	t.Helper()
	f := &fakeTUI{screen: "home"}
	in := strings.NewReader(strings.Join(lines, "\n") + "\n")
	var out strings.Builder
	if err := serve(f, d, in, &out, fastSettle()); err != nil {
		t.Fatalf("serve: %v", err)
	}
	return decodeResponses(t, out.String())
}

func mustBeOK(t *testing.T, resp map[string]any) {
	t.Helper()
	if ok, _ := resp["ok"].(bool); !ok {
		t.Fatalf("want ok:true, got %v", resp)
	}
}

func mustBeErr(t *testing.T, resp map[string]any, wantSubstr string) {
	t.Helper()
	if ok, _ := resp["ok"].(bool); ok {
		t.Fatalf("want error response, got ok:true: %v", resp)
	}
	msg, _ := resp["error"].(string)
	if wantSubstr != "" && !strings.Contains(msg, wantSubstr) {
		t.Fatalf("error %q missing %q", msg, wantSubstr)
	}
}

// TestDaemonMutatorsHappyPath drives all six mutator actions and asserts each
// entity is recorded on the fake with the mapped fields.
func TestDaemonMutatorsHappyPath(t *testing.T) {
	d := &fakeDaemon{}
	resps := runDaemonReqs(t, d,
		`{"id":1,"op":"daemon","action":"add_repo","repo":{"id":"repo-9","displayName":"Widgets"}}`,
		`{"id":2,"op":"daemon","action":"add_session","session":{"id":"sess-9","title":"Do the thing","state":"READY_FOR_REVIEW"}}`,
		`{"id":3,"op":"daemon","action":"add_chat","chat":{"id":"chat-9","sessionId":"sess-9","title":"A chat"}}`,
		`{"id":4,"op":"daemon","action":"add_cron_job","cronJob":{"id":"cron-9","name":"Nightly"}}`,
		`{"id":5,"op":"daemon","action":"add_prs","prs":{"repoId":"repo-9","prs":[{"number":42,"title":"Fix bug"}]}}`,
		`{"id":6,"op":"daemon","action":"add_tracker_issues","trackerIssues":{"repoId":"repo-9","issues":[{"externalId":"BOS-1","title":"Ticket"}]}}`,
	)
	if len(resps) != 6 {
		t.Fatalf("want 6 responses, got %d: %v", len(resps), resps)
	}
	for _, r := range resps {
		mustBeOK(t, r)
		if _, ok := r["screen"]; !ok {
			t.Fatalf("mutator response should carry a settled screen: %v", r)
		}
	}
	if len(d.repos) != 1 || d.repos[0].Id != "repo-9" || d.repos[0].DisplayName != "Widgets" {
		t.Fatalf("repo not recorded: %+v", d.repos)
	}
	if len(d.sessions) != 1 || d.sessions[0].Id != "sess-9" ||
		d.sessions[0].State != pb.SessionState_SESSION_STATE_READY_FOR_REVIEW {
		t.Fatalf("session not recorded/mapped: %+v", d.sessions)
	}
	if len(d.chats) != 1 || d.chats[0].SessionId != "sess-9" {
		t.Fatalf("chat not recorded: %+v", d.chats)
	}
	if len(d.crons) != 1 || d.crons[0].Name != "Nightly" {
		t.Fatalf("cron not recorded: %+v", d.crons)
	}
	if len(d.prs["repo-9"]) != 1 || d.prs["repo-9"][0].Number != 42 {
		t.Fatalf("prs not recorded: %+v", d.prs)
	}
	if len(d.issues["repo-9"]) != 1 || d.issues["repo-9"][0].ExternalId != "BOS-1" {
		t.Fatalf("issues not recorded: %+v", d.issues)
	}
}

// TestDaemonPushersHappyPath drives the three pushers against an attached
// session and asserts the fake recorded each push with the mapped states.
func TestDaemonPushersHappyPath(t *testing.T) {
	d := &fakeDaemon{attached: map[string]bool{"sess-x": true}}
	resps := runDaemonReqs(t, d,
		`{"id":1,"op":"daemon","action":"push_output","sessionId":"sess-x","text":"hello"}`,
		`{"id":2,"op":"daemon","action":"state_change","sessionId":"sess-x","previousState":"IMPLEMENTING_PLAN","newState":"READY_FOR_REVIEW"}`,
		`{"id":3,"op":"daemon","action":"session_ended","sessionId":"sess-x","finalState":"MERGED"}`,
	)
	if len(resps) != 3 {
		t.Fatalf("want 3 responses, got %d: %v", len(resps), resps)
	}
	for _, r := range resps {
		mustBeOK(t, r)
	}
	if len(d.outputs) != 1 || d.outputs[0].text != "hello" {
		t.Fatalf("output not pushed: %+v", d.outputs)
	}
	if len(d.stateChanges) != 1 ||
		d.stateChanges[0].prev != pb.SessionState_SESSION_STATE_IMPLEMENTING_PLAN ||
		d.stateChanges[0].next != pb.SessionState_SESSION_STATE_READY_FOR_REVIEW {
		t.Fatalf("state change not pushed/mapped: %+v", d.stateChanges)
	}
	if len(d.ended) != 1 || d.ended[0].final != pb.SessionState_SESSION_STATE_MERGED {
		t.Fatalf("session_ended not pushed: %+v", d.ended)
	}
}

// TestDaemonUnknownActionListsValid asserts an unknown action errors listing
// every valid action, sorted.
func TestDaemonUnknownActionListsValid(t *testing.T) {
	resps := runDaemonReqs(t, &fakeDaemon{},
		`{"id":1,"op":"daemon","action":"frobnicate"}`)
	if len(resps) != 1 {
		t.Fatalf("want 1 response, got %d", len(resps))
	}
	mustBeErr(t, resps[0], "unknown daemon action")
	msg, _ := resps[0]["error"].(string)
	for _, a := range []string{"add_session", "add_repo", "add_chat", "add_cron_job",
		"add_prs", "add_tracker_issues", "push_output", "state_change", "session_ended"} {
		if !strings.Contains(msg, a) {
			t.Fatalf("unknown-action error should list %q; got %q", a, msg)
		}
	}
}

// TestDaemonMutatorsMissingPayload asserts each mutator names its missing field.
func TestDaemonMutatorsMissingPayload(t *testing.T) {
	cases := map[string]string{
		"add_session":        "session",
		"add_repo":           "repo",
		"add_chat":           "chat",
		"add_cron_job":       "cronJob",
		"add_prs":            "prs",
		"add_tracker_issues": "trackerIssues",
	}
	for action, field := range cases {
		d := &fakeDaemon{}
		resps := runDaemonReqs(t, d,
			`{"id":1,"op":"daemon","action":"`+action+`"}`)
		mustBeErr(t, resps[0], field)
	}
}

// TestDaemonEntityRejectsUnknownField asserts the daemon op strict-decodes each
// nested entity payload (plan decision #8): an unknown field fails loudly naming
// it, never silently dropped, and the entity is not recorded.
func TestDaemonEntityRejectsUnknownField(t *testing.T) {
	cases := []struct{ action, line string }{
		{"add_session", `{"id":1,"op":"daemon","action":"add_session","session":{"id":"s","title":"t","bogusField":1}}`},
		{"add_repo", `{"id":1,"op":"daemon","action":"add_repo","repo":{"id":"r","displayName":"D","bogusField":1}}`},
		{"add_cron_job", `{"id":1,"op":"daemon","action":"add_cron_job","cronJob":{"id":"j","name":"n","bogusField":1}}`},
		{"add_prs", `{"id":1,"op":"daemon","action":"add_prs","prs":{"repoId":"r","bogusField":1}}`},
	}
	for _, tc := range cases {
		d := &fakeDaemon{}
		resps := runDaemonReqs(t, d, tc.line)
		mustBeErr(t, resps[0], "bogusField")
		if len(d.sessions)+len(d.repos)+len(d.crons)+len(d.prs) != 0 {
			t.Fatalf("%s with an unknown field must not record an entity", tc.action)
		}
	}
}

// TestDaemonMutatorNullPayload asserts an explicit JSON null payload is treated
// as missing (not a nil-deref), naming the field.
func TestDaemonMutatorNullPayload(t *testing.T) {
	resps := runDaemonReqs(t, &fakeDaemon{},
		`{"id":1,"op":"daemon","action":"add_session","session":null}`)
	mustBeErr(t, resps[0], "session")
}

// TestDaemonPushersRequireSessionID asserts each pusher errors on an empty
// sessionId and records no push.
func TestDaemonPushersRequireSessionID(t *testing.T) {
	for _, action := range []string{"push_output", "state_change", "session_ended"} {
		d := &fakeDaemon{}
		resps := runDaemonReqs(t, d,
			`{"id":1,"op":"daemon","action":"`+action+`"}`)
		mustBeErr(t, resps[0], "sessionId is required")
		if len(d.outputs)+len(d.stateChanges)+len(d.ended) != 0 {
			t.Fatalf("%s with empty sessionId should not push", action)
		}
	}
}

// TestDaemonBadStateNameSameError asserts state_change reuses the overlay
// state-name mapping: a bad name yields the same pointerful "unknown state"
// error, and validation happens before the attach check.
func TestDaemonBadStateNameSameError(t *testing.T) {
	d := &fakeDaemon{attached: map[string]bool{"sess-x": true}}
	resps := runDaemonReqs(t, d,
		`{"id":1,"op":"daemon","action":"state_change","sessionId":"sess-x","previousState":"NONSENSE","newState":"MERGED"}`)
	mustBeErr(t, resps[0], "unknown state")
	if len(d.stateChanges) != 0 {
		t.Fatalf("bad state name should not push: %+v", d.stateChanges)
	}
}

// TestDaemonPusherNotAttached asserts a pusher against a non-attached session
// errors with an actionable message and records no push (never a silent no-op).
func TestDaemonPusherNotAttached(t *testing.T) {
	d := &fakeDaemon{} // attached is nil ⇒ SessionAttached is always false
	resps := runDaemonReqs(t, d,
		`{"id":1,"op":"daemon","action":"state_change","sessionId":"sess-x","previousState":"IMPLEMENTING_PLAN","newState":"MERGED"}`)
	mustBeErr(t, resps[0], "not attached")
	if len(d.stateChanges) != 0 {
		t.Fatalf("push to non-attached session should be refused: %+v", d.stateChanges)
	}
}

// TestDaemonPusherTimeoutKeepsLoopAlive proves a pusher whose TryPush* returns a
// timeout error (simulating a full/undrained channel) yields an error response
// AND the serve loop still processes a subsequent request — it never hangs.
func TestDaemonPusherTimeoutKeepsLoopAlive(t *testing.T) {
	d := &fakeDaemon{
		attached: map[string]bool{"sess-x": true},
		pushErr:  errors.New("attach channel full; TUI not draining"),
	}
	resps := runDaemonReqs(t, d,
		`{"id":1,"op":"daemon","action":"push_output","sessionId":"sess-x","text":"hi"}`,
		`{"id":2,"op":"observe"}`,
	)
	if len(resps) != 2 {
		t.Fatalf("want 2 responses (error + live observe), got %d: %v", len(resps), resps)
	}
	mustBeErr(t, resps[0], "not draining")
	// The loop is proven alive by the second request being answered.
	if resps[1]["id"] != float64(2) || resps[1]["screen"] != "home" {
		t.Fatalf("serve loop did not survive the push timeout: %v", resps[1])
	}
}

// TestCapabilitiesOp asserts capabilities returns the sorted op and action lists,
// including the daemon op and every daemon action.
func TestCapabilitiesOp(t *testing.T) {
	resps := runDaemonReqs(t, &fakeDaemon{}, `{"id":1,"op":"capabilities"}`)
	if len(resps) != 1 {
		t.Fatalf("want 1 response, got %d", len(resps))
	}
	mustBeOK(t, resps[0])
	ops := toStringSlice(t, resps[0]["ops"])
	if !sortedContains(ops, "daemon") || !sortedContains(ops, "capabilities") || !sortedContains(ops, "observe") {
		t.Fatalf("ops missing expected entries: %v", ops)
	}
	if !isSorted(ops) {
		t.Fatalf("ops not sorted: %v", ops)
	}
	actions := toStringSlice(t, resps[0]["daemonActions"])
	if len(actions) != 9 || !isSorted(actions) {
		t.Fatalf("want 9 sorted daemon actions, got %v", actions)
	}
	for _, a := range []string{"add_session", "add_repo", "add_chat", "add_cron_job",
		"add_prs", "add_tracker_issues", "push_output", "state_change", "session_ended"} {
		if !sortedContains(actions, a) {
			t.Fatalf("daemonActions missing %q: %v", a, actions)
		}
	}
}

func toStringSlice(t *testing.T, v any) []string {
	t.Helper()
	raw, ok := v.([]any)
	if !ok {
		t.Fatalf("value is not a JSON array: %T %v", v, v)
	}
	out := make([]string, len(raw))
	for i, e := range raw {
		out[i], _ = e.(string)
	}
	return out
}

func sortedContains(xs []string, want string) bool {
	for _, x := range xs {
		if x == want {
			return true
		}
	}
	return false
}

func isSorted(xs []string) bool {
	for i := 1; i < len(xs); i++ {
		if xs[i-1] > xs[i] {
			return false
		}
	}
	return true
}
