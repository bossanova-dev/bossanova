package main

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"connectrpc.com/connect"

	"github.com/recurser/bossd/internal/adapter/credmaterialize"
	"github.com/recurser/bossd/internal/credsync"
	"github.com/recurser/bossd/internal/db"
	"github.com/recurser/bossd/internal/dbtest"
	"github.com/recurser/bossd/internal/domain"
)

func TestSyncedRefreshCannotBypassMissingCoordination(t *testing.T) {
	ctx := context.Background()
	store := db.NewAgentAccountStore(dbtest.New(t))
	a, err := store.Create(ctx, domain.CreateAgentAccountParams{Provider: "codex", Label: "shared"})
	if err != nil {
		t.Fatal(err)
	}
	if err = store.SetSync(ctx, a.ID, "synced", 1); err != nil {
		t.Fatal(err)
	}
	gate := daemonRefreshGate{w: &daemonWiring{accounts: store}}
	if _, err = gate.Acquire(ctx, a.ID, make([]byte, 32), "attempt"); credmaterialize.ClassifyRefreshError(err) != credmaterialize.RefreshFailureSyncUnavailable {
		t.Fatalf("synced credential acquired without coordination: %v", err)
	}
	if err = gate.Receive(ctx, a.ID); credmaterialize.ClassifyRefreshError(err) != credmaterialize.RefreshFailureSyncUnavailable {
		t.Fatalf("synced credential received without coordination: %v", err)
	}
	if err = gate.PublishRefresh(ctx, a.ID, nil, nil); credmaterialize.ClassifyRefreshError(err) != credmaterialize.RefreshFailureSyncUnavailable {
		t.Fatalf("synced credential published without coordination: %v", err)
	}
	local, err := store.Create(ctx, domain.CreateAgentAccountParams{Provider: "codex", Label: "local"})
	if err != nil {
		t.Fatal(err)
	}
	if acquired, err := gate.Acquire(ctx, local.ID, make([]byte, 32), "attempt"); err != nil || !acquired {
		t.Fatal("independent local account required cloud coordination")
	}
}

func TestRefreshGateClassifiesTheFailuresOnlyItCanSee_Accounts_AC223401(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want credmaterialize.RefreshFailureClass
	}{
		{"identity not locally pinned", errCredentialIdentityNotPinned, credmaterialize.RefreshFailureIdentityNotPinned},
		{"engine bootstrap not pinned", fmt.Errorf("receive: %w", credsync.ErrBootstrapNotPinned), credmaterialize.RefreshFailureIdentityNotPinned},
		{"device not enrolled", credsync.ErrOwnDeviceUnverified, credmaterialize.RefreshFailureDeviceNotEnrolled},
		{"sync unavailable", errCredentialSyncUnavailable, credmaterialize.RefreshFailureSyncUnavailable},
		{"bosso unavailable", connect.NewError(connect.CodeUnavailable, errors.New("dial")), credmaterialize.RefreshFailureSyncUnreachable},
		{"bosso deadline", connect.NewError(connect.CodeDeadlineExceeded, errors.New("slow")), credmaterialize.RefreshFailureSyncUnreachable},
		{"context deadline", fmt.Errorf("list devices: %w", context.DeadlineExceeded), credmaterialize.RefreshFailureSyncUnreachable},
		{"remote conflict", credsync.ErrConflict, credmaterialize.RefreshFailureCredentialChanged},
		{"credential replaced", credsync.ErrCredentialReplaced, credmaterialize.RefreshFailureCredentialChanged},
		{"version resolution", errors.New("credential sync: no published refresh version"), credmaterialize.RefreshFailureUnclassified},
		{"other connect status", connect.NewError(connect.CodePermissionDenied, errors.New("denied")), credmaterialize.RefreshFailureUnclassified},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := classifyRefreshGateError(tc.err)
			if class := credmaterialize.ClassifyRefreshError(got); class != tc.want {
				t.Fatalf("class = %q, want %q", class, tc.want)
			}
			if !errors.Is(got, tc.err) || got.Error() != tc.err.Error() {
				t.Fatalf("classification changed the error: %v", got)
			}
		})
	}
	if classifyRefreshGateError(nil) != nil {
		t.Fatal("a nil error gained a class")
	}
}
