package account

import (
	"bytes"
	"context"
	"strings"
	"testing"

	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/spf13/cobra"
)

type listCommandClient struct {
	Client
	stub *accountLSStub
}

func (c listCommandClient) ListAgentAccounts(ctx context.Context, provider string, refresh bool) ([]*pb.AgentAccount, error) {
	return c.stub.ListAgentAccounts(ctx, provider, refresh)
}

func TestNewKeepsClientFactoriesIndependent(t *testing.T) {
	first := &accountLSStub{accounts: []*pb.AgentAccount{{Id: "first-account"}}}
	second := &accountLSStub{accounts: []*pb.AgentAccount{{Id: "second-account"}}}
	var firstOutput, secondOutput bytes.Buffer
	firstCalls, secondCalls := 0, 0
	firstCmd := New(func(cmd *cobra.Command) (Client, error) {
		firstCalls++
		if cmd.Name() != "ls" {
			t.Fatalf("factory received %q", cmd.Name())
		}
		return listCommandClient{stub: first}, nil
	}, nil, nil)
	secondCmd := New(func(cmd *cobra.Command) (Client, error) {
		secondCalls++
		return listCommandClient{stub: second}, nil
	}, nil, nil)
	firstCmd.SetOut(&firstOutput)
	secondCmd.SetOut(&secondOutput)
	firstCmd.SetArgs([]string{"ls", "--json", "--provider", "claude", "--refresh"})
	secondCmd.SetArgs([]string{"ls", "--json", "--provider", "codex"})
	if err := firstCmd.Execute(); err != nil {
		t.Fatal(err)
	}
	if err := secondCmd.Execute(); err != nil {
		t.Fatal(err)
	}
	if firstCalls != 1 || secondCalls != 1 {
		t.Fatalf("factory calls: first=%d second=%d", firstCalls, secondCalls)
	}
	if first.provider != "claude" || !first.refresh || second.provider != "codex" || second.refresh {
		t.Fatalf("flags crossed command groups: first=%+v second=%+v", first, second)
	}
	if !strings.Contains(firstOutput.String(), "first-account") || strings.Contains(firstOutput.String(), "second-account") {
		t.Fatalf("first group output: %s", &firstOutput)
	}
	if !strings.Contains(secondOutput.String(), "second-account") || strings.Contains(secondOutput.String(), "first-account") {
		t.Fatalf("second group output: %s", &secondOutput)
	}
}
