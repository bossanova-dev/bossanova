//go:build e2e

package main

import (
	"context"
	"errors"
	"os"
	"strings"
	"sync"

	"connectrpc.com/connect"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
)

// e2eOrganizationState is the proof-only stand-in for bosso's organization and
// repo-organization-mapping RPCs. The repo settings view reaches these through a
// type assertion on the cloud access client, so hanging them off
// e2eCloudAccessClient is what makes the organization field appear at all in a
// replay: without them the assertion fails and the row is (correctly) hidden.
//
// The mapping is keyed by nothing — a fixture seeds one repo, so the fake
// answers the same mapping for every origin URL rather than making a scenario
// author guess the demo repo's canonical origin.
type e2eOrganizationState struct {
	mu sync.Mutex

	orgs []*pb.Organization
	// mapped is the organization id currently mapped, "" for unmapped
	// (the "Personal" default).
	mapped string
	// setRefusal, when set, is the connect code SetRepoOrganization answers
	// with instead of writing — the shape bosso returns when the caller is not
	// a member of the organization they named.
	setRefusal connect.Code
}

// resolveE2EOrganizationState builds the organization fake from the
// BOSS_ORG_E2E_* family, or returns nil when a scenario asked for none. A nil
// state still yields a usable client, but not a uniformly empty one: the two
// reads answer empty, which is the signed-in-but-no-organizations shape, while
// the two writes fail loudly rather than report having persisted something no
// scenario configured a store for.
func resolveE2EOrganizationState() *e2eOrganizationState {
	raw := strings.TrimSpace(os.Getenv("BOSS_ORG_E2E_ORGANIZATIONS"))
	mapped := strings.TrimSpace(os.Getenv("BOSS_ORG_E2E_MAPPING"))
	refusal := strings.TrimSpace(os.Getenv("BOSS_ORG_E2E_SET_ERROR"))
	if raw == "" && mapped == "" && refusal == "" {
		return nil
	}
	orgs := parseE2EOrganizations(raw)
	state := &e2eOrganizationState{orgs: orgs, mapped: resolveE2EOrgID(orgs, mapped)}
	switch refusal {
	case "permission_denied":
		state.setRefusal = connect.CodePermissionDenied
	case "failed_precondition":
		state.setRefusal = connect.CodeFailedPrecondition
	}
	return state
}

// parseE2EOrganizations reads a comma-separated list of "id=Name" pairs. A bare
// "Name" gets a slugged id, so the common fixture stays readable.
func parseE2EOrganizations(raw string) []*pb.Organization {
	var orgs []*pb.Organization
	for _, part := range strings.Split(raw, ",") {
		entry := strings.TrimSpace(part)
		if entry == "" {
			continue
		}
		id, name := "", entry
		if key, value, ok := strings.Cut(entry, "="); ok {
			id = strings.TrimSpace(key)
			name = strings.TrimSpace(value)
		}
		if name == "" {
			continue
		}
		if id == "" {
			id = "org-" + slugE2EOrgName(name)
		}
		orgs = append(orgs, &pb.Organization{Id: id, WorkosOrgId: id, Name: name})
	}
	return orgs
}

func slugE2EOrgName(name string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(name) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
		default:
			b.WriteRune('-')
		}
	}
	return strings.Trim(b.String(), "-")
}

// resolveE2EOrgID accepts either an organization id or its display name, so a
// scenario can seed BOSS_ORG_E2E_MAPPING=Acme. A value matching neither is
// returned unchanged — that is exactly how a scenario stages the mapping that
// names an organization the caller is not a member of.
func resolveE2EOrgID(orgs []*pb.Organization, value string) string {
	if value == "" {
		return ""
	}
	for _, org := range orgs {
		if org.GetId() == value || strings.EqualFold(org.GetName(), value) {
			return org.GetId()
		}
	}
	return value
}

func (c *e2eCloudAccessClient) ListOrganizations(context.Context) ([]*pb.Organization, error) {
	state := c.organizations
	if state == nil {
		return nil, nil
	}
	state.mu.Lock()
	defer state.mu.Unlock()
	out := make([]*pb.Organization, len(state.orgs))
	copy(out, state.orgs)
	return out, nil
}

func (c *e2eCloudAccessClient) GetRepoOrganization(_ context.Context, repoOriginURL string) (*pb.RepoOrganizationMapping, error) {
	state := c.organizations
	if state == nil {
		return nil, nil
	}
	state.mu.Lock()
	defer state.mu.Unlock()
	if state.mapped == "" {
		return nil, nil
	}
	return &pb.RepoOrganizationMapping{
		Id:             "mapping-e2e",
		RepoOriginUrl:  repoOriginURL,
		OrganizationId: state.mapped,
	}, nil
}

func (c *e2eCloudAccessClient) SetRepoOrganization(_ context.Context, repoOriginURL, organizationID string) (*pb.RepoOrganizationMapping, error) {
	state := c.organizations
	if state == nil {
		return nil, errors.New("e2e organizations not configured")
	}
	state.mu.Lock()
	defer state.mu.Unlock()
	if state.setRefusal != 0 {
		return nil, connect.NewError(state.setRefusal, errors.New("caller is not a member of that organization"))
	}
	state.mapped = organizationID
	return &pb.RepoOrganizationMapping{
		Id:             "mapping-e2e",
		RepoOriginUrl:  repoOriginURL,
		OrganizationId: organizationID,
	}, nil
}

func (c *e2eCloudAccessClient) ClearRepoOrganization(_ context.Context, _, _ string) error {
	state := c.organizations
	if state == nil {
		return errors.New("e2e organizations not configured")
	}
	state.mu.Lock()
	defer state.mu.Unlock()
	state.mapped = ""
	return nil
}
