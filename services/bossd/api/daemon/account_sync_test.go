package server

import (
	"context"
	"testing"

	"github.com/recurser/bossd/internal/domain"
)

func TestRefreshedCredentialPublicationCanInstallConflictWinner(t *testing.T) {
	creds := &lockedFakeCredStore{fakeCredStore: newFakeCredStore()}
	srv, _ := newAccountServer(t, creds, nil)
	account := &domain.AgentAccount{ID: "local", SyncedAgentAccountID: "synced"}
	published := 0
	srv.credentialPublisher = func(_ context.Context, synced, local string) error {
		published++
		if creds.lockHeld() {
			t.Error("publication holds the credential lock; installing a remote conflict winner would deadlock")
			return nil
		}
		if synced != account.SyncedAgentAccountID || local != account.ID {
			t.Fatalf("publication identifiers = %q, %q", synced, local)
		}
		// A rejected publication receives the winning remote credential and
		// installs it under the same lock used by the explicit refresh.
		return creds.WithCredentialLock(local, func() error {
			stored, err := creds.Load(local)
			if err != nil {
				return err
			}
			if string(stored) != "replacement" {
				t.Fatalf("publication read %q before the replacement was saved", stored)
			}
			return creds.Save(local, []byte("remote-winner"))
		})
	}
	if err := srv.saveRefreshedCredential(context.Background(), account, []byte("replacement")); err != nil {
		t.Fatal(err)
	}
	if published != 1 {
		t.Fatalf("publication calls = %d, want 1", published)
	}
	stored, err := creds.Load(account.ID)
	if err != nil || string(stored) != "remote-winner" {
		t.Fatalf("stored credential = %q, err = %v", stored, err)
	}
}

func TestAccountToProtoSyncMetadata(t *testing.T) {
	a := accountToProto(&domain.AgentAccount{ID: "local", SyncedAgentAccountID: "synced", SyncVersion: 23})
	if a.GetSyncedAgentAccountId() != "synced" || a.GetSyncVersion() != 23 {
		t.Fatalf("account=%+v", a)
	}
}
