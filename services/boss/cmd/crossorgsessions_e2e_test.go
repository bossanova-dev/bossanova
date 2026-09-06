//go:build e2e

package main

import (
	"context"
	"testing"

	"github.com/recurser/boss/internal/client"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
)

// stubCrossOrgClient is the inner client the seed decorates: it answers a fixed
// two-repo world so the seed's attribution and withholding are observable
// without a mock daemon.
type stubCrossOrgClient struct {
	client.BossClient
	sessions []*pb.Session
}

func (s *stubCrossOrgClient) ListSessionsWithReadFailures(context.Context, *pb.ListSessionsRequest, client.SessionReadOptions) ([]*pb.Session, []*pb.OrganizationSessionReadFailure, error) {
	return s.sessions, nil, nil
}

func crossOrgStub() *stubCrossOrgClient {
	return &stubCrossOrgClient{sessions: []*pb.Session{
		{Id: "sess-1", RepoId: "repo-1", RepoDisplayName: "my-app"},
		{Id: "sess-2", RepoId: "repo-3", RepoDisplayName: "my-web"},
	}}
}

func seedCrossOrgEnv(t *testing.T, orgs, sessionOrgs, failure string) {
	t.Helper()
	t.Setenv("BOSS_ORG_E2E_ORGANIZATIONS", orgs)
	t.Setenv("BOSS_ORG_E2E_SESSION_ORGS", sessionOrgs)
	t.Setenv("BOSS_ORG_E2E_SESSION_READ_FAILURE", failure)
}

// TestApplyE2ECrossOrgSessionsIsInertWithoutASeed pins the default: every
// scenario that never asked for a cross-organization world must get the client
// it already had, unwrapped, so no existing capture shifts.
func TestApplyE2ECrossOrgSessionsIsInertWithoutASeed(t *testing.T) {
	seedCrossOrgEnv(t, "", "", "")
	inner := crossOrgStub()
	if got := applyE2ECrossOrgSessions(inner); got != client.BossClient(inner) {
		t.Fatalf("applyE2ECrossOrgSessions wrapped the client with no seed: %T", got)
	}
}

// TestApplyE2ECrossOrgSessionsAttributesSessions pins the union scene's data:
// the served list spans both organizations and every session carries the
// organization its repo belongs to, so the union a scenario captures is a real
// one rather than one list relabelled.
func TestApplyE2ECrossOrgSessionsAttributesSessions(t *testing.T) {
	seedCrossOrgEnv(t, "org-acme=Acme,org-globex=Globex", "my-app=Acme,my-web=Globex", "")
	inner := crossOrgStub()
	decorated := applyE2ECrossOrgSessions(inner)

	sessions, failures, err := decorated.ListSessionsWithReadFailures(context.Background(), &pb.ListSessionsRequest{}, client.SessionReadOptions{})
	if err != nil {
		t.Fatalf("ListSessionsWithReadFailures: %v", err)
	}
	if len(failures) != 0 {
		t.Fatalf("a healthy union reported failures: %+v", failures)
	}
	got := map[string]string{}
	for _, sess := range sessions {
		got[sess.GetId()] = sess.GetOrganizationId()
	}
	if got["sess-1"] != "org-acme" || got["sess-2"] != "org-globex" {
		t.Fatalf("sessions not attributed across organizations: %+v", got)
	}
	// The seed must not mutate the world the mock daemon keeps handing back.
	for _, sess := range inner.sessions {
		if sess.GetOrganizationId() != "" {
			t.Fatalf("seed stamped the underlying world: %+v", sess)
		}
	}
}

// TestApplyE2ECrossOrgSessionsWithholdsTheFailedOrganization pins the partial
// scene: the failing organization's sessions are gone exactly as a failed
// fan-out branch loses them, the surviving organization's sessions are still
// served, and the failure is reported with the name and reason the notice puts
// on screen.
func TestApplyE2ECrossOrgSessionsWithholdsTheFailedOrganization(t *testing.T) {
	const reason = "sessions for this organization are temporarily unavailable"
	seedCrossOrgEnv(t, "org-acme=Acme,org-globex=Globex", "my-app=Acme,my-web=Globex", "Globex:"+reason)
	decorated := applyE2ECrossOrgSessions(crossOrgStub())

	sessions, failures, err := decorated.ListSessionsWithReadFailures(context.Background(), &pb.ListSessionsRequest{}, client.SessionReadOptions{})
	if err != nil {
		t.Fatalf("a partial read must not be an error: %v", err)
	}
	if len(sessions) != 1 || sessions[0].GetId() != "sess-1" {
		t.Fatalf("partial read did not serve exactly the readable organization: %+v", sessions)
	}
	if len(failures) != 1 {
		t.Fatalf("partial read reported %d failures, want 1", len(failures))
	}
	if failures[0].GetOrganizationId() != "org-globex" || failures[0].GetOrganizationName() != "Globex" {
		t.Fatalf("failure did not identify the organization: %+v", failures[0])
	}
	if failures[0].GetReason() != reason {
		t.Fatalf("failure reason = %q, want it carried verbatim", failures[0].GetReason())
	}

	// ListSessions is the same read minus the report, so the sessions-only
	// callers in a scene see the same withheld list.
	only, err := decorated.ListSessions(context.Background(), &pb.ListSessionsRequest{}, client.SessionReadOptions{})
	if err != nil {
		t.Fatalf("ListSessions: %v", err)
	}
	if len(only) != 1 {
		t.Fatalf("ListSessions served %d sessions, want the same 1", len(only))
	}
}
