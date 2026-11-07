package main

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"errors"

	"connectrpc.com/connect"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	server "github.com/recurser/bossd/api/daemon"
	"github.com/recurser/bossd/internal/adapter/migrate"
	"github.com/recurser/bossd/internal/db"
	"github.com/recurser/bossd/internal/domain"
	"github.com/recurser/bossd/internal/streamseal"
	"github.com/recurser/bossd/migrations"
)

func TestViewerApprovalWiring(t *testing.T) {
	database, err := db.OpenInMemory()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	if err := migrate.Run(database, migrations.FS); err != nil {
		t.Fatal(err)
	}
	key := make([]byte, 32)
	key[0] = 7
	registry := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer daemon-token" {
			t.Error("missing daemon bearer")
		}
		w.Header().Set("Content-Type", "application/json")
		if _, err := fmt.Fprintf(w, `{"viewerDevices":[{"id":"viewer","publicKey":%q,"label":"browser"}]}`, base64.StdEncoding.EncodeToString(key)); err != nil {
			t.Error(err)
		}
	}))
	defer registry.Close()
	wiring := &daemonWiring{viewerApprovalStore: db.NewViewerDeviceApprovalStore(database)}
	wiring.wireViewerApprovals(registry.Client(), registry.URL, func() string { return "daemon-token" }, ed25519.NewKeyFromSeed(make([]byte, ed25519.SeedSize)))
	service := server.New(server.Config{ViewerApprovals: wiring.viewerApprovals})
	ctx := context.Background()
	if _, err := service.ApproveViewerDevice(ctx, connect.NewRequest(&pb.ApproveViewerDeviceRequest{ViewerDeviceId: "viewer", Fingerprint: streamseal.ViewerFingerprint("viewer", key)})); err != nil {
		t.Fatal(err)
	}
	approval, err := wiring.viewerApprovals.Get(ctx, "viewer")
	if err != nil || approval.Label != "browser" || approval.PublicKey[0] != 7 {
		t.Fatalf("stored approval: %#v %v", approval, err)
	}
	revoked, cancel := wiring.viewerApprovals.Subscribe("viewer")
	defer cancel()
	if _, err := service.RevokeViewerDevice(ctx, connect.NewRequest(&pb.RevokeViewerDeviceRequest{ViewerDeviceId: "viewer"})); err != nil {
		t.Fatal(err)
	}
	if _, err := wiring.viewerApprovals.Get(ctx, "viewer"); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("revocation retained approval: %v", err)
	}
	select {
	case <-revoked:
	default:
		t.Fatal("no revocation signal")
	}
}
