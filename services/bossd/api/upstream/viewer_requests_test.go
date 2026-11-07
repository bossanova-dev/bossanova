package upstream

import (
	"context"
	"encoding/base64"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"connectrpc.com/connect"
)

func TestViewerRequestsRegistryWire(t *testing.T) {
	token := "first"
	key := make([]byte, 32)
	key[0] = 9
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/bossanova.v1.OrchestratorService/ListViewerDeviceRequests" || r.Method != "POST" {
			t.Errorf("route %s %s", r.Method, r.URL.Path)
		}
		if r.Header.Get("Authorization") != "Bearer "+token {
			t.Error("stale daemon token")
		}
		if r.Header.Get("Content-Type") != "application/json" {
			t.Error("not Connect JSON")
		}
		w.Header().Set("Content-Type", "application/json")
		if _, err := fmt.Fprintf(w, `{"viewerDevices":[{"id":"viewer","publicKey":%q,"label":"browser","createdAt":"2026-11-06T00:00:00Z","lastRequestedAt":"2026-11-06T00:00:00Z"}]}`, base64.StdEncoding.EncodeToString(key)); err != nil {
			t.Errorf("write registry response: %v", err)
		}
	}))
	defer srv.Close()
	read := NewViewerRequestReader(srv.Client(), srv.URL, func() string { return token })
	for _, next := range []string{"first", "rotated"} {
		token = next
		rows, err := read(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if len(rows) != 1 || rows[0].ID != "viewer" || len(rows[0].PublicKey) != 32 || rows[0].PublicKey[0] != 9 {
			t.Fatalf("registry rows: %#v", rows)
		}
	}
}
func TestViewerRequestsRejectUnavailableAndPropagateErrors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		if _, err := fmt.Fprint(w, `{"code":"unauthenticated","message":"expired"}`); err != nil {
			t.Errorf("write registry error: %v", err)
		}
	}))
	defer srv.Close()
	for _, tc := range []struct {
		token string
		code  connect.Code
	}{{"", connect.CodeFailedPrecondition}, {"expired", connect.CodeUnauthenticated}} {
		read := NewViewerRequestReader(srv.Client(), srv.URL, func() string { return tc.token })
		_, err := read(context.Background())
		if connect.CodeOf(err) != tc.code {
			t.Fatalf("error: %v", err)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := NewViewerRequestReader(srv.Client(), srv.URL, func() string { return "token" })(ctx)
	if connect.CodeOf(err) != connect.CodeCanceled {
		t.Fatalf("canceled request: %v", err)
	}
}
