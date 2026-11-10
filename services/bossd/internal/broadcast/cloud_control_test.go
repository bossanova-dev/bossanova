package broadcast

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/recurser/bossalib/models"

	"github.com/recurser/bossd/internal/domain"
	"github.com/rs/zerolog"
)

func TestCloudDeliveryRechecksPolicyAndKeepsIdentity_Broadcasts_AC238101(t *testing.T) {
	store := newFakeStore()
	clk := newClock(time.Now().UTC())
	store.addBroadcast("hello", clk.now().Add(time.Hour), "chat-1")
	store.broadcasts[testBroadcastID].IsCloudControlled = true
	deliverer := newCaptureDeliverer(nil)
	allowed := false
	var checked []string
	worker := NewDeliveryWorker(WorkerConfig{Store: store, Deliverer: deliverer, Now: clk.now, Owner: testOwnerPrefix, Logger: zerolog.Nop(), CloudContext: func(ctx context.Context, chatID string) (context.Context, error) {
		checked = append(checked, chatID)
		if !allowed {
			return ctx, domain.ErrCloudTarget
		}
		return ctx, nil
	}})
	worker.scan(context.Background())
	first := store.delivery(t, "b-1-d1")
	if deliverer.count() != 0 || first.State != domain.BroadcastDeliveryStatePending {
		t.Fatal("queued cloud delivery typed after downgrade")
	}
	allowed = true
	clk.advance(time.Minute)
	worker.scan(context.Background())
	second := store.delivery(t, "b-1-d1")
	if deliverer.count() != 1 || second.State != domain.BroadcastDeliveryStateDelivered || first.ID != second.ID || len(checked) != 2 || checked[0] != "chat-1" || checked[1] != "chat-1" {
		t.Fatalf("retry changed delivery identity: %v", checked)
	}
}

func TestLocalBroadcastKeepsLocalAuthority(t *testing.T) {
	store := newFakeStore()
	clk := newClock(time.Now().UTC())
	store.addBroadcast("hello", clk.now().Add(time.Hour), "chat-1")
	deliverer := newCaptureDeliverer(nil)
	worker := NewDeliveryWorker(WorkerConfig{Store: store, Deliverer: deliverer, Now: clk.now, Owner: testOwnerPrefix, Logger: zerolog.Nop(), CloudContext: func(ctx context.Context, _ string) (context.Context, error) {
		t.Fatal("local delivery consulted cloud control")
		return ctx, domain.ErrCloudTarget
	}})
	worker.scan(context.Background())
	if deliverer.count() != 1 {
		t.Fatal("local delivery refused")
	}
}

func TestCloudAudienceCapExcludesProtectedChats(t *testing.T) {
	chats := []*domain.AgentChat{chat("allowed", "sa", "claude", "", "")}
	for i := 0; i <= MaxTargets; i++ {
		chats = append(chats, chat(fmt.Sprintf("protected-%d", i), "sb", "claude", "", ""))
	}
	resolver := NewResolver(&fakeChats{chats: chats}, &fakeSessions{sessions: map[string]*models.Session{"sa": {ID: "sa", RepoID: "a"}, "sb": {ID: "sb", RepoID: "b"}}}, zerolog.Nop())
	ctx := domain.WithCloudRepoCheck(domain.WithCloudTarget(context.Background(), domain.CloudTarget{"repo:a": "a", "repo:b": "b"}), func(_ context.Context, id string) bool { return id == "a" })
	targets, err := resolver.Resolve(ctx, mustParse(t, "repo:a+repo:b"), "", false)
	if err != nil || len(targets) != 1 || targets[0].ChatID != "allowed" {
		t.Fatalf("protected audience changed public result: targets=%v err=%v", targets, err)
	}
}
