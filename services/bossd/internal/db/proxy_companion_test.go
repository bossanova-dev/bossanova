package db

import (
	"context"
	"testing"
)

func TestProxyTokenCompanionShape(t *testing.T) {
	database := setupTestDB(t)
	sessionID := createProxyTokenTestSession(t, database, "companion")
	store := NewProxyTokenStore(database)
	rec := ProxyTokenRecord{TokenSHA256: hexDigest('a'), SessionID: sessionID, AccountID: "account-A", IsCompanionShaped: true}
	if err := store.Upsert(context.Background(), rec); err != nil {
		t.Fatal(err)
	}
	rec.AccountID = "account-B"
	if err := store.Upsert(context.Background(), rec); err != nil {
		t.Fatal(err)
	}
	got, err := store.GetByTokenHash(context.Background(), rec.TokenSHA256)
	if err != nil {
		t.Fatal(err)
	}
	if got == nil || !got.IsCompanionShaped || got.IsChatShaped || got.AccountID != "account-B" {
		t.Fatal("companion shape or binding failed to round-trip")
	}
	rec.IsChatShaped = true
	rec.AgentSessionID = "chat-1"
	if err := store.Upsert(context.Background(), rec); err == nil {
		t.Fatal("accepted both chat and companion shapes")
	}
}
