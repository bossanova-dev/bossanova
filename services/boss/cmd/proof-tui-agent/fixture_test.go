package main

import (
	"context"
	"testing"

	"github.com/recurser/boss/internal/adapter/realstackrpc"
	"github.com/recurser/boss/internal/fixtures"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"google.golang.org/protobuf/proto"
)

func TestFixturePRsUseGitHubBoundary(t *testing.T) {
	if testing.Short() {
		t.Skip("real daemon fixtures: go test ./cmd/proof-tui-agent -run TestFixturePRsUseGitHubBoundary")
	}
	f := startFixtureStack(t)
	const prNumber int32 = 42
	if err := f.seedWorld(context.Background(), fixtures.World{
		Repos:    []*pb.Repo{{Id: "repo", DisplayName: "fixture"}},
		Sessions: []*pb.Session{{Id: "session", RepoId: "repo", Title: "Seeded pull request", State: pb.SessionState_SESSION_STATE_READY_FOR_REVIEW, PrNumber: proto.Int32(prNumber)}},
	}); err != nil {
		t.Fatal(err)
	}
	session, err := f.stack.Client().GetSession(context.Background(), realstackrpc.Request(&pb.GetSessionRequest{Id: f.sessions["session"]}))
	if err != nil {
		t.Fatal(err)
	}
	if got := session.Msg.GetSession().GetPrNumber(); got != prNumber {
		t.Fatalf("seeded session PR=%d want %d", got, prNumber)
	}
	if err := f.seedPRs([]fixtures.OverlayRepoPRs{{RepoID: "repo", PRs: []fixtures.OverlayPR{{Number: 77, Title: "Overlay pull request", HeadBranch: "overlay"}}}}); err != nil {
		t.Fatal(err)
	}
	response, err := f.stack.Client().ListRepoPRs(context.Background(), realstackrpc.Request(&pb.ListRepoPRsRequest{RepoId: f.repos["repo"]}))
	if err != nil {
		t.Fatal(err)
	}
	for _, pr := range response.Msg.PullRequests {
		if pr.Number == 77 && pr.Title == "Overlay pull request" {
			return
		}
	}
	t.Fatalf("overlay PR not returned by real daemon: %v", response.Msg.PullRequests)
}

func TestFixtureAccountsPreserveSupersededVerdict(t *testing.T) {
	if testing.Short() {
		t.Skip("real daemon fixtures: go test ./cmd/proof-tui-agent -run TestFixtureAccountsPreserveSupersededVerdict")
	}
	f := startFixtureStack(t)
	world := fixtures.SupersededCredentialWorld()
	if err := f.seedAccounts(context.Background(), world.Accounts); err != nil {
		t.Fatal(err)
	}
	response, err := f.stack.Client().ListAgentAccounts(context.Background(), realstackrpc.Request(&pb.ListAgentAccountsRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	if got := len(response.Msg.Accounts); got != len(world.Accounts) {
		t.Fatalf("account count=%d want %d", got, len(world.Accounts))
	}
	for _, account := range response.Msg.Accounts {
		if account.Label != "shared-codex" {
			continue
		}
		if account.GetAuthCheck().GetOutcome() != "healthy" || account.GetAuthCheck().GetFailureClass() != "credential_superseded" {
			t.Fatalf("superseded account verdict=%v", account.AuthCheck)
		}
		return
	}
	t.Fatal("superseded account missing from real daemon inventory")
}

func TestFixtureRepoOrganizationPreservesOrigin(t *testing.T) {
	if testing.Short() {
		t.Skip("real daemon fixtures: go test ./cmd/proof-tui-agent -run TestFixtureRepoOrganizationPreservesOrigin")
	}
	f := startFixtureStack(t)
	world := fixtures.RepoOrganizationWorld()
	if err := f.seedWorld(context.Background(), world); err != nil {
		t.Fatal(err)
	}
	for _, want := range world.Sessions {
		if want.PrNumber == nil {
			continue
		}
		got, err := f.stack.Client().GetSession(context.Background(), realstackrpc.Request(&pb.GetSessionRequest{Id: f.sessions[want.Id]}))
		if err != nil {
			t.Fatal(err)
		}
		if got.Msg.GetSession().GetPrNumber() != want.GetPrNumber() {
			t.Fatalf("repository organization session PR=%d want %d", got.Msg.GetSession().GetPrNumber(), want.GetPrNumber())
		}
	}
	response, err := f.stack.Client().ListRepos(context.Background(), realstackrpc.Request(&pb.ListReposRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range world.Repos {
		found := false
		for _, got := range response.Msg.Repos {
			if got.Id == f.repos[want.Id] {
				found = true
				if got.OriginUrl != want.OriginUrl || got.SentryOrg != want.SentryOrg {
					t.Fatalf("repo metadata=%v want origin %q sentry org %q", got, want.OriginUrl, want.SentryOrg)
				}
			}
		}
		if !found {
			t.Fatalf("missing repository %s", want.Id)
		}
	}
}
