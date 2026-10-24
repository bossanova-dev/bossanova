//go:build e2e

package settings

import (
	"context"
	"errors"
	"os"
	"strings"
	"sync"

	sessioncli "github.com/recurser/boss/api/cli/session"

	"github.com/recurser/boss/internal/domain/rpcerr"
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
	setRefusal rpcerr.Kind
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
	orgs := sessioncli.ParseE2EOrganizations(raw)
	state := &e2eOrganizationState{orgs: orgs, mapped: sessioncli.ResolveE2EOrganizationID(orgs, mapped)}
	switch refusal {
	case "permission_denied":
		state.setRefusal = rpcerr.PermissionDenied
	case "failed_precondition":
		state.setRefusal = rpcerr.FailedPrecondition
	}
	return state
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
	if state.setRefusal != "" {
		return nil, rpcerr.New(state.setRefusal, errors.New("caller is not a member of that organization"))
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
