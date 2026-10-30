package proxy

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/recurser/bossd/internal/db"
	"github.com/recurser/bossd/internal/domain"
	"github.com/rs/zerolog"
)

func TestCompanionTokensDurableRebind_Accounts_AC30(t *testing.T) {
	sqlDB := rebuildTestDB(t)
	sessionID := rebuildTestSession(t, sqlDB)
	store := db.NewProxyTokenStore(sqlDB)
	p := newRebuildProxy(t, store, &bytes.Buffer{})
	first := p.TokenForCompanion(sessionID, "account-A")
	if first == "" {
		t.Fatal("mint failed")
	}
	p.rememberBearer(domain.ProxyTargetForCompanion(sessionID), "stale-bearer")
	p.RebindCompanion(sessionID, "account-B")
	if p.bearerForSession(domain.ProxyTargetForCompanion(sessionID)) != "" {
		t.Fatal("rebind kept stale bearer")
	}
	if again := p.TokenForCompanion(sessionID, "account-A"); again != first {
		t.Fatal("respawn changed live token")
	}
	if account, ok := p.CompanionAgentAccount(sessionID); !ok || account != "account-B" {
		t.Fatal("respawn reset rotated binding")
	}

	restarted := newRebuildProxy(t, store, &bytes.Buffer{})
	if err := restarted.RebuildTokenRegistry(context.Background()); err != nil {
		t.Fatal(err)
	}
	if target, ok := restarted.TokenTarget(first); !ok || target != domain.ProxyTargetForCompanion(sessionID) {
		t.Fatal("restart lost target")
	}
	if account, ok := restarted.CompanionAgentAccount(sessionID); !ok || account != "account-B" {
		t.Fatal("restart lost binding")
	}
	second := restarted.TokenForCompanion(sessionID, "account-A")
	if second == "" || second == first {
		t.Fatal("restart must mint without recovering a raw token")
	}
	restarted.RebindCompanion(sessionID, "account-C")
	rows, err := store.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 {
		t.Fatalf("rows=%d, want both generations", len(rows))
	}
	for _, row := range rows {
		if !row.IsCompanionShaped || row.IsChatShaped || row.AccountID != "account-C" {
			t.Fatal("rebind did not update every companion row")
		}
	}
	if got := passthroughDisplayID(domain.ProxyTargetForCompanion(sessionID)); got != sessionID {
		t.Fatal("display exposed internal target")
	}
	restarted.Deregister(sessionID)
	if _, ok := restarted.CompanionAgentAccount(sessionID); ok {
		t.Fatal("deregister retained binding")
	}
	_, firstExists := restarted.TokenTarget(first)
	_, secondExists := restarted.TokenTarget(second)
	if firstExists || secondExists {
		t.Fatal("deregister retained token")
	}
	rows, err = store.List(context.Background())
	if err != nil || len(rows) != 0 {
		t.Fatal("deregister retained durable rows")
	}
}

func TestCompanionSentinelReplaysWithNextBearer_Accounts_AC30(t *testing.T) {
	up, srv := newFakeUpstream(t)
	f := &fakeFailover{currentBearer: "first-token", result: domain.FailoverResult{Rotate: true, Token: "second-token", NextAccountID: "account-B", Trigger: "ROTATION_TRIGGER_USAGE_LIMITED"}}
	p, base := startProxy(t, f, srv.URL, zerolog.Nop())
	token := p.TokenForCompanion("session-1", "account-A")
	req, err := http.NewRequest(http.MethodPost, base+"/s/"+token+"/v1/messages", strings.NewReader(`{"prompt":"hello"}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("x-api-key", domain.SentinelAPIKey)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d, want replay success", resp.StatusCode)
	}
	requests := up.all()
	if len(requests) != 2 {
		t.Fatalf("requests=%d, want original and replay", len(requests))
	}
	if requests[0].auth != "Bearer first-token" || requests[1].auth != "Bearer second-token" {
		t.Fatal("incorrect account bearers")
	}
	for _, request := range requests {
		if request.apiKey != "" {
			t.Fatal("sentinel reached upstream")
		}
	}
	if f.currentBearerID != domain.ProxyTargetForCompanion("session-1") || f.commitCalls != 1 {
		t.Fatal("companion target did not traverse failover")
	}
}
