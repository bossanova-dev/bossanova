//go:build e2e

package main

import (
	"context"
	"os"
	"strings"

	"github.com/recurser/boss/internal/client"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"google.golang.org/protobuf/proto"
)

// e2eCrossOrgSessions is the proof-only stand-in for the cross-organization
// session read (BOS-1151). The TUI e2e harness drives boss against a local mock
// daemon, and a local daemon by construction reads one place and never reports a
// partial read — so without this the two screens that only exist for a
// multi-organization cloud read (a union spanning organizations, and a union one
// organization short) are unreachable in a replay.
//
// It decorates the real BossClient rather than replacing it, so every other RPC
// in the scene still goes to the mock daemon and the board a scenario captures
// is the production board.
type e2eCrossOrgSessions struct {
	client.BossClient

	// sessionOrgs maps a repo id or repo display name to the organization id
	// its sessions belong to. A cloud read is a fan-out over organizations, and
	// a repo origin maps to exactly one organization, so attributing sessions by
	// repo is how a seeded union stays coherent with the rest of the world.
	sessionOrgs map[string]string
	// failedOrg is the organization whose read fails, "" when none does. Its
	// sessions are withheld exactly as a failed fan-out branch withholds them —
	// the point of the scene is that the rest of the list still arrives.
	failedOrg *pb.OrganizationSessionReadFailure
}

// applyE2ECrossOrgSessions wraps c so the home list reads as a cross-organization
// union, when a scenario asked for one. Returns c untouched otherwise, so every
// existing scenario is byte-unaffected.
func applyE2ECrossOrgSessions(c client.BossClient) client.BossClient {
	if c == nil {
		return c
	}
	orgs := parseE2EOrganizations(strings.TrimSpace(os.Getenv("BOSS_ORG_E2E_ORGANIZATIONS")))
	sessionOrgs := parseE2ESessionOrgs(os.Getenv("BOSS_ORG_E2E_SESSION_ORGS"), orgs)
	failure := parseE2ESessionReadFailure(os.Getenv("BOSS_ORG_E2E_SESSION_READ_FAILURE"), orgs)
	if len(sessionOrgs) == 0 && failure == nil {
		return c
	}
	return &e2eCrossOrgSessions{BossClient: c, sessionOrgs: sessionOrgs, failedOrg: failure}
}

// parseE2ESessionOrgs reads a comma-separated list of "repo=organization"
// assignments, where repo is a repo id or display name and organization is an id
// or display name from BOSS_ORG_E2E_ORGANIZATIONS. Naming the organizations once,
// in the key the rest of the family already uses, keeps a scene from spelling the
// same organization two ways and quietly seeding three organizations.
func parseE2ESessionOrgs(raw string, orgs []*pb.Organization) map[string]string {
	out := map[string]string{}
	for _, part := range strings.Split(raw, ",") {
		entry := strings.TrimSpace(part)
		if entry == "" {
			continue
		}
		repo, org, ok := strings.Cut(entry, "=")
		repo, org = strings.TrimSpace(repo), strings.TrimSpace(org)
		if !ok || repo == "" || org == "" {
			continue
		}
		out[repo] = resolveE2EOrgID(orgs, org)
	}
	return out
}

// parseE2ESessionReadFailure reads "organization" or "organization:reason". The
// reason is carried verbatim because the production notice carries the server's
// reason verbatim; a scene that could not stage a real-looking one would be
// proving a placeholder rather than the screen.
func parseE2ESessionReadFailure(raw string, orgs []*pb.Organization) *pb.OrganizationSessionReadFailure {
	entry := strings.TrimSpace(raw)
	if entry == "" {
		return nil
	}
	name, reason, _ := strings.Cut(entry, ":")
	name, reason = strings.TrimSpace(name), strings.TrimSpace(reason)
	if name == "" {
		return nil
	}
	id := resolveE2EOrgID(orgs, name)
	failure := &pb.OrganizationSessionReadFailure{OrganizationId: id, Reason: reason}
	for _, org := range orgs {
		if org.GetId() == id {
			failure.OrganizationName = org.GetName()
			break
		}
	}
	return failure
}

// sessionOrgID reports which organization a session belongs to under the seed.
func (c *e2eCrossOrgSessions) sessionOrgID(sess *pb.Session) string {
	if org, ok := c.sessionOrgs[sess.GetRepoId()]; ok {
		return org
	}
	return c.sessionOrgs[sess.GetRepoDisplayName()]
}

func (c *e2eCrossOrgSessions) ListSessions(ctx context.Context, req *pb.ListSessionsRequest, opts client.SessionReadOptions) ([]*pb.Session, error) {
	sessions, _, err := c.ListSessionsWithReadFailures(ctx, req, opts)
	return sessions, err
}

func (c *e2eCrossOrgSessions) ListSessionsWithReadFailures(ctx context.Context, req *pb.ListSessionsRequest, opts client.SessionReadOptions) ([]*pb.Session, []*pb.OrganizationSessionReadFailure, error) {
	sessions, failures, err := c.BossClient.ListSessionsWithReadFailures(ctx, req, opts)
	if err != nil {
		return nil, nil, err
	}
	failedID := c.failedOrg.GetOrganizationId()
	served := make([]*pb.Session, 0, len(sessions))
	for _, sess := range sessions {
		org := c.sessionOrgID(sess)
		if failedID != "" && org == failedID {
			continue
		}
		if org != "" {
			// Copy before stamping: the mock daemon hands out its own records,
			// and a scene must not mutate the world every later poll re-reads.
			// proto.CloneOf, not *sess: a protobuf message carries an internal
			// mutex, so copying it by value is a copylocks violation.
			clone := proto.CloneOf(sess)
			clone.OrganizationId = org
			sess = clone
		}
		served = append(served, sess)
	}
	if c.failedOrg != nil {
		failures = append(failures, c.failedOrg)
	}
	return served, failures, nil
}
