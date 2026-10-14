package server

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"connectrpc.com/connect"
	"github.com/rs/zerolog"

	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/models"
	"github.com/recurser/bossd/internal/db"
	"github.com/recurser/bossd/internal/plugin"
	"github.com/recurser/bossd/internal/session"
)

// --- TrackerResolver ---

func resolverOver(sources ...plugin.TaskSource) *TrackerResolver {
	return &TrackerResolver{sources: func() []plugin.TaskSource { return sources }}
}

func TestTrackerResolver_ExactMatchReturnsTrackerURL(t *testing.T) {
	stub := &stubTaskSource{issuesToReturn: []*pb.TrackerIssue{
		{ExternalId: "BOS-15390", Url: "https://linear.app/acme/issue/BOS-15390"},
		{ExternalId: "BOS-1539", Url: "https://linear.app/acme/issue/BOS-1539/add-approve"},
	}}
	repo := &models.Repo{OriginURL: "https://github.com/acme/repo", LinearAPIKey: "lin_key"}

	id, url, err := resolverOver(stub).ResolveTrackerIssue(context.Background(), repo, "bos-1539")
	if err != nil {
		t.Fatalf("ResolveTrackerIssue: %v", err)
	}
	if id != "BOS-1539" || url != "https://linear.app/acme/issue/BOS-1539/add-approve" {
		t.Fatalf("got (%q, %q)", id, url)
	}
	if stub.receivedQuery != "BOS-1539" || stub.receivedConfig["linear_api_key"] != "lin_key" || stub.receivedRepoOriginURL != repo.OriginURL {
		t.Fatalf("plugin got query=%q config=%v origin=%q", stub.receivedQuery, stub.receivedConfig, stub.receivedRepoOriginURL)
	}
}

// TestTrackerResolver_OnlyResolutionSkipsPRMatching pins the split: the
// resolver asks the plugin to skip the open-PR listing entirely, while the
// picker path (ListTrackerIssues passes trackerSource's config to the plugin
// unchanged) does not, so the New Session picker still gets PR matches and
// sees a listing failure. The literal is the wire value the plugin reads.
func TestTrackerResolver_OnlyResolutionSkipsPRMatching(t *testing.T) {
	stub := &stubTaskSource{issuesToReturn: []*pb.TrackerIssue{
		{ExternalId: "BOS-1539", Url: "https://linear.app/acme/issue/BOS-1539/add-approve"},
	}}
	repo := &models.Repo{OriginURL: "https://github.com/acme/repo", LinearAPIKey: "lin_key"}

	if _, _, err := resolverOver(stub).ResolveTrackerIssue(context.Background(), repo, "BOS-1539"); err != nil {
		t.Fatalf("ResolveTrackerIssue: %v", err)
	}
	if got := stub.receivedConfig["pr_matching"]; got != "skip" {
		t.Fatalf("resolver config pr_matching = %q, want %q (config %v)", got, "skip", stub.receivedConfig)
	}

	sources := func() []plugin.TaskSource { return []plugin.TaskSource{stub} }
	_, config, err := trackerSource(context.Background(), sources, repo, "linear")
	if err != nil {
		t.Fatalf("trackerSource: %v", err)
	}
	if _, ok := config["pr_matching"]; ok {
		t.Fatalf("picker config carries pr_matching (%v); a listing failure must fail ListTrackerIssues", config)
	}
}

func TestTrackerResolver_Errors(t *testing.T) {
	keyed := &models.Repo{LinearAPIKey: "lin_key"}
	tests := []struct {
		name     string
		resolver *TrackerResolver
		repo     *models.Repo
		want     error
		wantMsg  string
	}{
		{name: "no linear key", resolver: resolverOver(&stubTaskSource{}), repo: &models.Repo{}, want: session.ErrTrackerNotConfigured},
		{name: "no plugin host", resolver: NewTrackerResolver(nil), repo: keyed, want: session.ErrTrackerNotConfigured},
		{name: "no linear plugin", resolver: resolverOver(&stubTaskSource{name: "sentry"}), repo: keyed, want: session.ErrTrackerNotConfigured},
		{
			name: "no exact match",
			resolver: resolverOver(&stubTaskSource{issuesToReturn: []*pb.TrackerIssue{
				{ExternalId: "BOS-15390", Url: "https://linear.app/acme/issue/BOS-15390"},
			}}),
			repo: keyed,
			want: session.ErrTrackerIssueNotFound,
		},
		{
			name:     "plugin error is wrapped as is",
			resolver: resolverOver(&stubTaskSource{errToReturn: errors.New("linear 500")}),
			repo:     keyed,
			wantMsg:  "linear 500",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, _, err := tt.resolver.ResolveTrackerIssue(context.Background(), tt.repo, "BOS-1539")
			if err == nil {
				t.Fatal("expected an error")
			}
			if tt.want != nil && !errors.Is(err, tt.want) {
				t.Fatalf("err = %v, want %v", err, tt.want)
			}
			if tt.wantMsg != "" {
				if !strings.Contains(err.Error(), tt.wantMsg) || errors.Is(err, session.ErrTrackerNotConfigured) || errors.Is(err, session.ErrTrackerIssueNotFound) {
					t.Fatalf("err = %v, want plugin error %q", err, tt.wantMsg)
				}
			}
		})
	}
}

// --- SetSessionTracker ---

type fakeServerTrackerResolver struct {
	issues map[string]string
	err    error
	calls  []string
}

func (f *fakeServerTrackerResolver) ResolveTrackerIssue(_ context.Context, _ *models.Repo, id string) (string, string, error) {
	f.calls = append(f.calls, id)
	if f.err != nil {
		return "", "", f.err
	}
	url, ok := f.issues[id]
	if !ok {
		return "", "", fmt.Errorf("%w: %s", session.ErrTrackerIssueNotFound, id)
	}
	return id, url, nil
}

type setTrackerHarness struct {
	srv      *Server
	sessions db.SessionStore
	resolver *fakeServerTrackerResolver
	updated  []*pb.Session
	sessID   string
}

func newSetTrackerHarness(t *testing.T) *setTrackerHarness {
	t.Helper()
	sqlDB := setupServerTestDB(t)
	repos := db.NewRepoStore(sqlDB)
	sessions := db.NewSessionStore(sqlDB)
	ctx := context.Background()
	repo, err := repos.Create(ctx, db.CreateRepoParams{
		DisplayName:       "repo",
		LocalPath:         "/tmp/repo",
		OriginURL:         "https://github.com/acme/repo",
		DefaultBaseBranch: "main",
		WorktreeBaseDir:   "/tmp/wt",
	})
	if err != nil {
		t.Fatalf("create repo: %v", err)
	}
	key := "lin_key"
	if _, err := repos.Update(ctx, repo.ID, db.UpdateRepoParams{LinearAPIKey: &key}); err != nil {
		t.Fatalf("set linear key: %v", err)
	}
	sess, err := sessions.Create(ctx, db.CreateSessionParams{RepoID: repo.ID, Title: "s", BranchName: "feature", BaseBranch: "main"})
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	h := &setTrackerHarness{
		sessions: sessions,
		resolver: &fakeServerTrackerResolver{issues: map[string]string{
			"BOS-1539": "https://linear.app/acme/issue/BOS-1539/add-approve",
			"BOS-1540": "https://linear.app/acme/issue/BOS-1540/other",
		}},
		sessID: sess.ID,
	}
	h.srv = &Server{
		repos:           repos,
		sessions:        sessions,
		trackerResolver: h.resolver,
		logger:          zerolog.Nop(),
		onSessionUpdated: func(_ context.Context, p *pb.Session) {
			h.updated = append(h.updated, p)
		},
	}
	return h
}

func (h *setTrackerHarness) call(id, trackerID string) (*pb.Session, error) {
	resp, err := h.srv.SetSessionTracker(context.Background(), connect.NewRequest(&pb.SetSessionTrackerRequest{Id: id, TrackerId: trackerID}))
	if err != nil {
		return nil, err
	}
	return resp.Msg.GetSession(), nil
}

func TestSetSessionTracker_LinksAndCorrects(t *testing.T) {
	h := newSetTrackerHarness(t)

	got, err := h.call(h.sessID, " bos-1539 ")
	if err != nil {
		t.Fatalf("SetSessionTracker: %v", err)
	}
	if got.GetTrackerId() != "BOS-1539" || got.GetTrackerUrl() != "https://linear.app/acme/issue/BOS-1539/add-approve" {
		t.Fatalf("returned session tracker = (%q, %q)", got.GetTrackerId(), got.GetTrackerUrl())
	}
	if got.GetRepoDisplayName() != "repo" {
		t.Fatalf("returned session is not hydrated: repo display name %q", got.GetRepoDisplayName())
	}
	if len(h.updated) != 1 || h.updated[0].GetTrackerId() != "BOS-1539" {
		t.Fatalf("onSessionUpdated calls = %v", h.updated)
	}

	// Overwriting an existing link is the correction path.
	got, err = h.call(h.sessID, "BOS-1540")
	if err != nil {
		t.Fatalf("SetSessionTracker overwrite: %v", err)
	}
	if got.GetTrackerId() != "BOS-1540" || got.GetTrackerUrl() != "https://linear.app/acme/issue/BOS-1540/other" {
		t.Fatalf("overwrite returned (%q, %q)", got.GetTrackerId(), got.GetTrackerUrl())
	}
	stored, err := h.sessions.Get(context.Background(), h.sessID)
	if err != nil {
		t.Fatalf("reload session: %v", err)
	}
	if stored.TrackerID == nil || *stored.TrackerID != "BOS-1540" || stored.TrackerURL == nil || *stored.TrackerURL != "https://linear.app/acme/issue/BOS-1540/other" {
		t.Fatalf("stored tracker = (%v, %v)", stored.TrackerID, stored.TrackerURL)
	}
	if len(h.updated) != 2 {
		t.Fatalf("onSessionUpdated calls = %d, want 2", len(h.updated))
	}
}

func TestSetSessionTracker_ErrorCodes(t *testing.T) {
	tests := []struct {
		name      string
		id        func(h *setTrackerHarness) string
		trackerID string
		resolver  error
		wantCode  connect.Code
		wantMsg   string
	}{
		{name: "missing id", id: func(*setTrackerHarness) string { return "" }, trackerID: "BOS-1539", wantCode: connect.CodeInvalidArgument},
		{name: "bad tracker id", id: func(h *setTrackerHarness) string { return h.sessID }, trackerID: "not-an-id!", wantCode: connect.CodeInvalidArgument},
		{name: "unknown session", id: func(*setTrackerHarness) string { return "nope" }, trackerID: "BOS-1539", wantCode: connect.CodeNotFound},
		{name: "issue not found", id: func(h *setTrackerHarness) string { return h.sessID }, trackerID: "BOS-404", wantCode: connect.CodeNotFound, wantMsg: "not found among the tracker's open issues"},
		{name: "not configured", id: func(h *setTrackerHarness) string { return h.sessID }, trackerID: "BOS-1539", resolver: session.ErrTrackerNotConfigured, wantCode: connect.CodeFailedPrecondition, wantMsg: "no Linear tracker configured for this repo"},
		{name: "plugin failure", id: func(h *setTrackerHarness) string { return h.sessID }, trackerID: "BOS-1539", resolver: errors.New("linear 500"), wantCode: connect.CodeInternal},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newSetTrackerHarness(t)
			h.resolver.err = tt.resolver
			_, err := h.call(tt.id(h), tt.trackerID)
			if got := connect.CodeOf(err); got != tt.wantCode {
				t.Fatalf("code = %v (err %v), want %v", got, err, tt.wantCode)
			}
			if tt.wantMsg != "" && !strings.Contains(err.Error(), tt.wantMsg) {
				t.Fatalf("err = %v, want message containing %q", err, tt.wantMsg)
			}
			if len(h.updated) != 0 {
				t.Fatalf("onSessionUpdated fired on error: %v", h.updated)
			}
			stored, getErr := h.sessions.Get(context.Background(), h.sessID)
			if getErr != nil {
				t.Fatalf("reload: %v", getErr)
			}
			if stored.TrackerURL != nil && *stored.TrackerURL != "" {
				t.Fatalf("link written on error: %v", *stored.TrackerURL)
			}
		})
	}
}
