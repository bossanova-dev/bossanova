package account

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	pb "github.com/recurser/bossalib/gen/bossanova/v1"
)

// accountTestRemedyStub answers TestAgentAccount with a fixed auth check; every
// other Client method is unused by `boss account test`.
type accountTestRemedyStub struct {
	Client
	check *pb.AuthCheck
}

func (s *accountTestRemedyStub) TestAgentAccount(_ context.Context, id string) (*pb.TestAgentAccountResponse, error) {
	return &pb.TestAgentAccountResponse{
		Account:      &pb.AgentAccount{Id: id, LastTestError: "credential verification failed", AuthCheck: s.check},
		LiveSmokeRan: true,
	}, nil
}

func runAccountTestWith(t *testing.T, check *pb.AuthCheck) string {
	t.Helper()
	g := &group{newClient: func(*cobra.Command) (Client, error) { return &accountTestRemedyStub{check: check}, nil }}
	cmd := &cobra.Command{Use: "test"}
	cmd.Flags().Bool("json", false, "")
	cmd.SetContext(context.Background())
	var out bytes.Buffer
	cmd.SetOut(&out)
	if err := g.runAccountTest(cmd, "acct-1"); err != nil {
		t.Fatalf("runAccountTest: %v", err)
	}
	return out.String()
}

func TestAccountTestPrintsRemedyOnlyForBrokerFailureClasses_Accounts_AC223403(t *testing.T) {
	for _, tc := range []struct {
		class string
		want  string
	}{
		{"credential_device_not_enrolled", "Remedy: run `boss account devices approve` on an enrolled machine\n"},
		{"credential_identity_not_pinned", "Remedy: confirm the bootstrap fingerprint with `boss account devices approve`\n"},
		{"refresh_sync_unavailable", "Remedy: check `boss account devices ls`\n"},
		{"refresh_rejected", "Remedy: run `boss account reauth acct-1`\n"},
		{"refresh_owned_by_peer", "Remedy: retry shortly"},
	} {
		got := runAccountTestWith(t, &pb.AuthCheck{Outcome: "transient", FailureClass: tc.class})
		if !strings.Contains(got, tc.want) {
			t.Errorf("class %q printed %q; want it to contain %q", tc.class, got, tc.want)
		}
	}
	for _, check := range []*pb.AuthCheck{
		nil,
		{Outcome: "transient", FailureClass: "unclassified"},
		{Outcome: "transient", FailureClass: "rate_limited"},
		{Outcome: "auth_invalid", FailureClass: "auth_invalidated"},
		{Outcome: "unavailable", FailureClass: "runner_unavailable"},
		{Outcome: "healthy", FailureClass: "credential_superseded"},
	} {
		if got := runAccountTestWith(t, check); strings.Contains(got, "Remedy:") {
			t.Errorf("class %q printed a remedy: %q", check.GetFailureClass(), got)
		}
	}
}
