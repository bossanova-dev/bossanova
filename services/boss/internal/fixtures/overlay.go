package fixtures

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	pb "github.com/recurser/bossalib/gen/bossanova/v1"
)

// Overlay is a MINIMAL, display-focused description of extra entities to seed
// into the proof mock daemon on top of a preset world (via the bridge's -seed
// flag) or to push mid-run (Task 3's daemon op). Only fields the home board and
// its pickers actually render are exposed; everything richer stays a Go preset
// (Overlay hard rule, BOS-217 plan). Timestamps are NEVER absolute — they are
// integer offset-minutes from fixtures.fixedNow, mapped through ts(), so seeded
// screens stay byte-stable.
//
// STRICT DECODE (BOS-217 outside-voice #8): ParseOverlay decodes with
// json.Decoder.DisallowUnknownFields — DELIBERATELY stricter than the bridge's
// permissive NDJSON request parsing. An agent-authored -seed file with a typo'd
// field must fail loudly at boot (naming the field) rather than silently seeding
// nothing.
type Overlay struct {
	Repos         []OverlayRepo       `json:"repos,omitempty"`
	Sessions      []OverlaySession    `json:"sessions,omitempty"`
	Chats         []OverlayChat       `json:"chats,omitempty"`
	CronJobs      []OverlayCronJob    `json:"cronJobs,omitempty"`
	PRs           []OverlayRepoPRs    `json:"prs,omitempty"`
	TrackerIssues []OverlayRepoIssues `json:"trackerIssues,omitempty"`
}

// OverlaySession is the minimal display shape of a pb.Session. state accepts
// either the short form ("READY_FOR_REVIEW") or the full enum name
// ("SESSION_STATE_READY_FOR_REVIEW"); absent state defaults to UNSPECIFIED.
type OverlaySession struct {
	ID                string `json:"id"`
	RepoID            string `json:"repoId,omitempty"`
	RepoDisplayName   string `json:"repoDisplayName,omitempty"`
	Title             string `json:"title"`
	State             string `json:"state,omitempty"`
	PRNumber          *int   `json:"prNumber,omitempty"`
	BranchName        string `json:"branchName,omitempty"`
	CreatedOffsetMins int    `json:"createdOffsetMins,omitempty"`
}

// Build validates required fields and maps the overlay onto a *pb.Session,
// defaulting everything not display-relevant. Shared by -seed and the daemon op
// so there is a single validation path.
func (s OverlaySession) Build() (*pb.Session, error) {
	if s.ID == "" {
		return nil, fmt.Errorf("session: id is required")
	}
	if s.Title == "" {
		return nil, fmt.Errorf("session %q: title is required", s.ID)
	}
	state, err := SessionStateFromName(s.State)
	if err != nil {
		return nil, fmt.Errorf("session %q: %w", s.ID, err)
	}
	sess := &pb.Session{
		Id:              s.ID,
		RepoId:          s.RepoID,
		RepoDisplayName: s.RepoDisplayName,
		Title:           s.Title,
		BranchName:      s.BranchName,
		State:           state,
		CreatedAt:       ts(time.Duration(s.CreatedOffsetMins) * time.Minute),
	}
	if s.PRNumber != nil {
		if *s.PRNumber < 0 || *s.PRNumber > math.MaxInt32 {
			return nil, fmt.Errorf("session %q: prNumber %d out of int32 range", s.ID, *s.PRNumber)
		}
		n := int32(*s.PRNumber)
		sess.PrNumber = &n
	}
	return sess, nil
}

// OverlayRepo is the minimal display shape of a pb.Repo.
type OverlayRepo struct {
	ID                string `json:"id"`
	DisplayName       string `json:"displayName"`
	LocalPath         string `json:"localPath,omitempty"`
	DefaultBaseBranch string `json:"defaultBaseBranch,omitempty"`
	MergeStrategy     string `json:"mergeStrategy,omitempty"`
}

// Build validates required fields and defaults defaultBaseBranch="main" and
// mergeStrategy="merge".
func (r OverlayRepo) Build() (*pb.Repo, error) {
	if r.ID == "" {
		return nil, fmt.Errorf("repo: id is required")
	}
	if r.DisplayName == "" {
		return nil, fmt.Errorf("repo %q: displayName is required", r.ID)
	}
	base := r.DefaultBaseBranch
	if base == "" {
		base = "main"
	}
	strategy := r.MergeStrategy
	if strategy == "" {
		strategy = "merge"
	}
	return &pb.Repo{
		Id:                r.ID,
		DisplayName:       r.DisplayName,
		LocalPath:         r.LocalPath,
		DefaultBaseBranch: base,
		MergeStrategy:     strategy,
	}, nil
}

// OverlayChat is the minimal display shape of a pb.ClaudeChat.
type OverlayChat struct {
	ID                string `json:"id"`
	SessionID         string `json:"sessionId"`
	Title             string `json:"title"`
	AgentSessionID    string `json:"agentSessionId,omitempty"`
	CreatedOffsetMins int    `json:"createdOffsetMins,omitempty"`
}

// Build validates required fields and maps onto a *pb.ClaudeChat.
func (c OverlayChat) Build() (*pb.ClaudeChat, error) {
	if c.ID == "" {
		return nil, fmt.Errorf("chat: id is required")
	}
	if c.SessionID == "" {
		return nil, fmt.Errorf("chat %q: sessionId is required", c.ID)
	}
	if c.Title == "" {
		return nil, fmt.Errorf("chat %q: title is required", c.ID)
	}
	return &pb.ClaudeChat{
		Id:             c.ID,
		SessionId:      c.SessionID,
		Title:          c.Title,
		AgentSessionId: c.AgentSessionID,
		CreatedAt:      ts(time.Duration(c.CreatedOffsetMins) * time.Minute),
	}, nil
}

// OverlayCronJob is the minimal display shape of a pb.CronJob. Enabled is a
// *bool so an absent JSON value defaults to true (a plain bool would default to
// the zero value false); pass "enabled": false to disable explicitly.
type OverlayCronJob struct {
	ID          string `json:"id"`
	RepoID      string `json:"repoId,omitempty"`
	Name        string `json:"name"`
	Prompt      string `json:"prompt,omitempty"`
	Schedule    string `json:"schedule,omitempty"`
	Timezone    string `json:"timezone,omitempty"`
	Enabled     *bool  `json:"enabled,omitempty"`
	AgentName   string `json:"agentName,omitempty"`
	GateCommand string `json:"gateCommand,omitempty"`
}

// Build validates required fields and defaults schedule="@daily",
// timezone="UTC", agentName="claude", enabled=true (nil).
func (j OverlayCronJob) Build() (*pb.CronJob, error) {
	if j.ID == "" {
		return nil, fmt.Errorf("cronJob: id is required")
	}
	if j.Name == "" {
		return nil, fmt.Errorf("cronJob %q: name is required", j.ID)
	}
	schedule := j.Schedule
	if schedule == "" {
		schedule = "@daily"
	}
	timezone := j.Timezone
	if timezone == "" {
		timezone = "UTC"
	}
	agent := j.AgentName
	if agent == "" {
		agent = "claude"
	}
	enabled := true
	if j.Enabled != nil {
		enabled = *j.Enabled
	}
	return &pb.CronJob{
		Id:          j.ID,
		RepoId:      j.RepoID,
		Name:        j.Name,
		Prompt:      j.Prompt,
		Schedule:    schedule,
		Timezone:    timezone,
		Enabled:     enabled,
		AgentName:   agent,
		GateCommand: j.GateCommand,
	}, nil
}

// OverlayRepoPRs groups a repo's pull-request summaries (the pb store keys PRs
// by repo ID). repoId is required so the picker can attach them to a repo.
type OverlayRepoPRs struct {
	RepoID string      `json:"repoId"`
	PRs    []OverlayPR `json:"prs,omitempty"`
}

// OverlayPR is the minimal display shape of a pb.PRSummary. state accepts the
// short ("OPEN") or full ("PR_STATE_OPEN") form; absent state defaults to OPEN.
type OverlayPR struct {
	Number     int    `json:"number"`
	Title      string `json:"title"`
	State      string `json:"state,omitempty"`
	Author     string `json:"author,omitempty"`
	HeadBranch string `json:"headBranch,omitempty"`
}

// Build validates and maps a single PR onto a *pb.PRSummary.
func (p OverlayPR) Build() (*pb.PRSummary, error) {
	if p.Number <= 0 {
		return nil, fmt.Errorf("pr: number is required (must be > 0)")
	}
	if p.Title == "" {
		return nil, fmt.Errorf("pr #%d: title is required", p.Number)
	}
	state, err := prStateFromName(p.State)
	if err != nil {
		return nil, fmt.Errorf("pr #%d: %w", p.Number, err)
	}
	if p.Number < 0 || p.Number > math.MaxInt32 {
		return nil, fmt.Errorf("pr #%d: number out of int32 range", p.Number)
	}
	return &pb.PRSummary{
		Number:     int32(p.Number),
		Title:      p.Title,
		State:      state,
		Author:     p.Author,
		HeadBranch: p.HeadBranch,
	}, nil
}

// Build validates the group's repoId and every PR, returning the repo ID and
// the built summaries for MockDaemon.AddPRs.
func (g OverlayRepoPRs) Build() (string, []*pb.PRSummary, error) {
	if g.RepoID == "" {
		return "", nil, fmt.Errorf("prs: repoId is required")
	}
	out := make([]*pb.PRSummary, 0, len(g.PRs))
	for _, p := range g.PRs {
		built, err := p.Build()
		if err != nil {
			return "", nil, fmt.Errorf("prs for repo %q: %w", g.RepoID, err)
		}
		out = append(out, built)
	}
	return g.RepoID, out, nil
}

// OverlayRepoIssues groups a repo's tracker (Linear) issues, keyed by repo ID.
type OverlayRepoIssues struct {
	RepoID string                `json:"repoId"`
	Issues []OverlayTrackerIssue `json:"issues,omitempty"`
}

// OverlayTrackerIssue is the minimal display shape of a pb.TrackerIssue.
type OverlayTrackerIssue struct {
	ExternalID string `json:"externalId"`
	Title      string `json:"title"`
	State      string `json:"state,omitempty"`
	URL        string `json:"url,omitempty"`
	BranchName string `json:"branchName,omitempty"`
	PRNumber   int    `json:"prNumber,omitempty"`
}

// Build validates and maps a single tracker issue onto a *pb.TrackerIssue.
// state is a free-form string on pb.TrackerIssue (no enum), so it passes through.
func (i OverlayTrackerIssue) Build() (*pb.TrackerIssue, error) {
	if i.ExternalID == "" {
		return nil, fmt.Errorf("trackerIssue: externalId is required")
	}
	if i.Title == "" {
		return nil, fmt.Errorf("trackerIssue %q: title is required", i.ExternalID)
	}
	if i.PRNumber < 0 || i.PRNumber > math.MaxInt32 {
		return nil, fmt.Errorf("trackerIssue %q: prNumber %d out of int32 range", i.ExternalID, i.PRNumber)
	}
	return &pb.TrackerIssue{
		ExternalId: i.ExternalID,
		Title:      i.Title,
		State:      i.State,
		Url:        i.URL,
		BranchName: i.BranchName,
		PrNumber:   int32(i.PRNumber),
	}, nil
}

// Build validates the group's repoId and every issue, returning the repo ID and
// the built issues for MockDaemon.AddTrackerIssues.
func (g OverlayRepoIssues) Build() (string, []*pb.TrackerIssue, error) {
	if g.RepoID == "" {
		return "", nil, fmt.Errorf("trackerIssues: repoId is required")
	}
	out := make([]*pb.TrackerIssue, 0, len(g.Issues))
	for _, i := range g.Issues {
		built, err := i.Build()
		if err != nil {
			return "", nil, fmt.Errorf("trackerIssues for repo %q: %w", g.RepoID, err)
		}
		out = append(out, built)
	}
	return g.RepoID, out, nil
}

// ParseOverlay decodes an overlay JSON document with DisallowUnknownFields (see
// the Overlay doc for the strict-decode rationale). Unknown fields — top-level
// or nested — surface a "json: unknown field \"x\"" error that names the field.
func ParseOverlay(data []byte) (Overlay, error) {
	var o Overlay
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&o); err != nil {
		return Overlay{}, fmt.Errorf("parse overlay: %w", err)
	}
	if dec.More() {
		return Overlay{}, fmt.Errorf("parse overlay: unexpected trailing JSON after the overlay object")
	}
	return o, nil
}

// overlayTarget is the subset of *tuitest.MockDaemon that ApplyOverlay mutates.
// Declaring it here (rather than importing tuitest) keeps the fixtures package
// pb-only and preserves the "any test package can import fixtures without a
// cycle" invariant; *tuitest.MockDaemon satisfies it structurally, so main.go
// passes the daemon directly.
type overlayTarget interface {
	AddRepo(*pb.Repo)
	AddSession(*pb.Session)
	AddChat(*pb.ClaudeChat)
	AddCronJob(*pb.CronJob)
	AddPRs(repoID string, prs []*pb.PRSummary)
	AddTrackerIssues(repoID string, issues []*pb.TrackerIssue)
}

// ApplyOverlay validates and applies every overlay entity to the target,
// returning the first validation error (naming the offending entity/field).
// Repos are applied first so PR/tracker groups can reference them.
func ApplyOverlay(d overlayTarget, o Overlay) error {
	for _, r := range o.Repos {
		repo, err := r.Build()
		if err != nil {
			return err
		}
		d.AddRepo(repo)
	}
	for _, s := range o.Sessions {
		sess, err := s.Build()
		if err != nil {
			return err
		}
		d.AddSession(sess)
	}
	for _, c := range o.Chats {
		chat, err := c.Build()
		if err != nil {
			return err
		}
		d.AddChat(chat)
	}
	for _, j := range o.CronJobs {
		job, err := j.Build()
		if err != nil {
			return err
		}
		d.AddCronJob(job)
	}
	for _, g := range o.PRs {
		repoID, prs, err := g.Build()
		if err != nil {
			return err
		}
		d.AddPRs(repoID, prs)
	}
	for _, g := range o.TrackerIssues {
		repoID, issues, err := g.Build()
		if err != nil {
			return err
		}
		d.AddTrackerIssues(repoID, issues)
	}
	return nil
}

// SessionStateFromName maps a short ("READY_FOR_REVIEW") or full
// ("SESSION_STATE_READY_FOR_REVIEW") state name to the pb enum. Empty defaults
// to UNSPECIFIED. Unknown names return an error listing the valid short names.
// Exported so the proof-tui-agent daemon op's state_change/session_ended actions
// reuse the SAME overlay state-name mapping instead of duplicating it (BOS-217).
func SessionStateFromName(name string) (pb.SessionState, error) {
	if name == "" {
		return pb.SessionState_SESSION_STATE_UNSPECIFIED, nil
	}
	full := name
	if !strings.HasPrefix(full, "SESSION_STATE_") {
		full = "SESSION_STATE_" + full
	}
	if v, ok := pb.SessionState_value[full]; ok {
		return pb.SessionState(v), nil
	}
	return 0, fmt.Errorf("unknown state %q (valid: %s)", name, strings.Join(validEnumShortNames(pb.SessionState_value, "SESSION_STATE_"), ", "))
}

// prStateFromName maps a short ("OPEN") or full ("PR_STATE_OPEN") PR state name
// to the pb enum. Empty defaults to OPEN (the common display case). Unknown
// names return an error listing the valid short names.
func prStateFromName(name string) (pb.PRState, error) {
	if name == "" {
		return pb.PRState_PR_STATE_OPEN, nil
	}
	full := name
	if !strings.HasPrefix(full, "PR_STATE_") {
		full = "PR_STATE_" + full
	}
	if v, ok := pb.PRState_value[full]; ok {
		return pb.PRState(v), nil
	}
	return 0, fmt.Errorf("unknown pr state %q (valid: %s)", name, strings.Join(validEnumShortNames(pb.PRState_value, "PR_STATE_"), ", "))
}

// validEnumShortNames returns the sorted short (prefix-stripped) enum names,
// excluding the zero-value UNSPECIFIED, for a pb "_value" map. Used to build
// self-explanatory error messages.
func validEnumShortNames(values map[string]int32, prefix string) []string {
	names := make([]string, 0, len(values))
	for full, v := range values {
		if v == 0 {
			continue // skip UNSPECIFIED
		}
		names = append(names, strings.TrimPrefix(full, prefix))
	}
	sort.Strings(names)
	return names
}
