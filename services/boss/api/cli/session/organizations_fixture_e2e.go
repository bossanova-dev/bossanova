//go:build e2e

package session

import (
	"strings"

	pb "github.com/recurser/bossalib/gen/bossanova/v1"
)

// ParseE2EOrganizations reads a comma-separated list of "id=Name" pairs. A bare
// "Name" gets a slugged id, so the common fixture stays readable.
func ParseE2EOrganizations(raw string) []*pb.Organization {
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

// ResolveE2EOrganizationID accepts either an organization id or its display name, so a
// scenario can seed BOSS_ORG_E2E_MAPPING=Acme. A value matching neither is
// returned unchanged — that is exactly how a scenario stages the mapping that
// names an organization the caller is not a member of.
func ResolveE2EOrganizationID(orgs []*pb.Organization, value string) string {
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
