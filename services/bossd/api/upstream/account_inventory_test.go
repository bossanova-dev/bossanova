package upstream

import (
	"context"
	"testing"
	"time"

	"github.com/recurser/bossalib/kit/safego"
	"github.com/rs/zerolog"

	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"google.golang.org/protobuf/proto"
)

func TestAccountInventorySnapshotAndRefresh(t *testing.T) {
	accounts := []*pb.AgentAccount{{Id: "local", Label: "before", Identity: &pb.AgentAccountIdentity{Kind: "codex_user", Digest: "hashed", IsConfident: true}}}
	reader := AgentAccountSnapshotReaderFunc(func(context.Context) ([]*pb.AgentAccount, error) { return accounts, nil })
	client := newTestClient(StreamStores{AgentAccounts: reader})
	snap, err := client.buildSnapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(snap.AgentAccounts) != 1 || !proto.Equal(snap.AgentAccounts[0], accounts[0]) {
		t.Fatalf("snapshot inventory = %v", snap.AgentAccounts)
	}
	accounts = []*pb.AgentAccount{{Id: "local", Label: "after"}}
	out := make(chan *pb.DaemonEvent, 1)
	client.forwardEvent(context.Background(), StreamEvent{AgentAccounts: &AgentAccountsEvent{}}, out, nil)
	if got := (<-out).GetAgentAccountInventory(); got == nil || len(got.Accounts) != 1 || got.Accounts[0].Label != "after" {
		t.Fatalf("inventory = %v", got)
	}
	accounts = nil
	client.forwardEvent(context.Background(), StreamEvent{AgentAccounts: &AgentAccountsEvent{}}, out, nil)
	if got := (<-out).GetAgentAccountInventory(); got == nil || len(got.Accounts) != 0 {
		t.Fatalf("empty inventory = %v", got)
	}
}

func TestAccountInventoryEmptyReconnect(t *testing.T) {
	stream := newBlockingDaemonStream()
	client := newTestClient(StreamStores{AgentAccounts: AgentAccountSnapshotReaderFunc(func(context.Context) ([]*pb.AgentAccount, error) { return nil, nil })})
	client.opener = &blockingDaemonOpener{stream: stream}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	done := safego.Go(zerolog.Nop(), func() { _ = client.openStream(ctx) })
	defer func() { cancel(); <-done }()
	for i := 0; i < 2; i++ {
		select {
		case event := <-stream.sent:
			if i == 0 && event.GetSnapshot() == nil {
				t.Fatalf("first event = %T", event.Event)
			}
			if i == 1 && event.GetAgentAccountInventory() == nil {
				t.Fatalf("second event = %T", event.Event)
			}
		case <-ctx.Done():
			t.Fatal("missing connect inventory", ctx.Err())
		}
	}
}
