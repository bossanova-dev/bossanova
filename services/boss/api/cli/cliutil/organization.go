package cliutil

import (
	"context"
	"errors"
	"fmt"
	"strings"

	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/spf13/cobra"
)

type OrganizationLister interface {
	ListOrganizations(context.Context) ([]*pb.Organization, error)
}

func ResolveOrganizationID(cmd *cobra.Command, c OrganizationLister) (string, error) {
	if v, _ := cmd.Flags().GetString("org"); strings.TrimSpace(v) != "" {
		return strings.TrimSpace(v), nil
	}
	orgs, err := c.ListOrganizations(cmd.Context())
	if err != nil {
		return "", fmt.Errorf("list organizations to default --org: %w", err)
	}
	switch len(orgs) {
	case 0:
		return "", errors.New("you are not a member of any organization")
	case 1:
		return orgs[0].GetId(), nil
	}
	var b strings.Builder
	b.WriteString("you belong to several organizations; pass --org <id>:")
	for _, org := range orgs {
		fmt.Fprintf(&b, "\n  %s  %s", org.GetId(), org.GetName())
	}
	return "", errors.New(b.String())
}
