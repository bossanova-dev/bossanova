package main

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/recurser/boss/internal/adapter/realstackrpc"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// seedAccounts uses the account API for registration and the offline store path
// for historical usage and auth verdicts which no public mutation can set.
func (f *fixtureStack) seedAccounts(ctx context.Context, accounts []*pb.AgentAccount) error {
	if len(accounts) == 0 {
		return nil
	}
	rows := make([]map[string]any, 0, len(accounts))
	for _, account := range accounts {
		// These deliberately invalid provider credentials are fixture data. They
		// live only in this stack's file keyring and are never tested against a provider.
		credential := []byte("proof-fixture-only-" + account.Label)
		if account.Provider == "codex" {
			var err error
			credential, err = json.Marshal(map[string]string{"access": "proof-fixture-only-access-" + account.Label, "refresh": "proof-fixture-only-refresh-" + account.Label, "id_token": "proof-fixture-only-id-" + account.Label})
			if err != nil {
				return err
			}
		}
		created, err := f.stack.Client().AddAgentAccount(ctx, realstackrpc.Request(&pb.AddAgentAccountRequest{Provider: account.Provider, Label: account.Label, Priority: account.Priority, Credential: credential}))
		if err != nil {
			return fmt.Errorf("register fixture account %q: %w", account.Label, err)
		}
		id := created.Msg.GetAccount().GetId()
		if _, err = f.stack.Client().UpdateAgentAccount(ctx, realstackrpc.Request(&pb.UpdateAgentAccountRequest{Id: id, Status: &account.Status, AllowedModels: account.AllowedModels})); err != nil {
			return fmt.Errorf("update fixture account %q: %w", account.Label, err)
		}
		row := map[string]any{"id": id, "health": account.Health, "tier": account.Tier, "cooldown_until": fixtureTime(account.CooldownUntil), "last_used_at": fixtureTime(account.LastUsedAt), "last_test_ok_at": fixtureTime(account.LastTestOkAt), "last_test_error": account.LastTestError}
		if check := account.AuthCheck; check != nil {
			row["auth_check"] = map[string]any{"outcome": check.Outcome, "failure_class": check.FailureClass, "checked_at": fixtureTime(check.CheckedAt), "next_retry_at": fixtureTime(check.NextRetryAt)}
		}
		if usage := account.Usage; usage != nil {
			row["usage"] = map[string]any{"util_5h": usage.Util_5H, "util_7d": usage.Util_7D, "reset_5h": fixtureTime(usage.Reset_5H), "reset_7d": fixtureTime(usage.Reset_7D), "status": usage.Status, "plan_tier": usage.PlanTier, "fetched_at": fixtureTime(usage.FetchedAt)}
		}
		rows = append(rows, row)
	}
	spec, err := json.Marshal(map[string]any{"version": 1, "accounts": rows})
	if err != nil {
		return err
	}
	_, err = f.stack.Seed(spec)
	return err
}

func fixtureTime(stamp *timestamppb.Timestamp) *time.Time {
	if stamp == nil {
		return nil
	}
	value := stamp.AsTime()
	return &value
}
