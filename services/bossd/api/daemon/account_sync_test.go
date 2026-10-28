package server

import (
	"testing"

	"github.com/recurser/bossd/internal/domain"
)

func TestAccountToProtoSyncMetadata(t *testing.T) {
	a := accountToProto(&domain.AgentAccount{ID: "local", SyncedAgentAccountID: "synced", SyncVersion: 23})
	if a.GetSyncedAgentAccountId() != "synced" || a.GetSyncVersion() != 23 {
		t.Fatalf("account=%+v", a)
	}
}
