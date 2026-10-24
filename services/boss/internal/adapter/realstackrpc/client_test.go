package realstackrpc

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/recurser/bossalib/contract/daemon/socketauth"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/gen/bossanova/v1/bossanovav1connect"
	"google.golang.org/protobuf/proto"
)

func TestClientAuthenticatesDaemonRequests(t *testing.T) {
	token := strings.Repeat("01", 32)
	payload, err := proto.Marshal(&pb.ListReposResponse{Repos: []*pb.Repo{{Id: "isolated-repo", DisplayName: "fixture"}}})
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !socketauth.ValidHTTPBearer(r, token) {
			http.Error(w, "unauthenticated", http.StatusUnauthorized)
			return
		}
		if r.URL.Path != bossanovav1connect.DaemonServiceListReposProcedure {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/proto")
		_, _ = w.Write(payload)
	}))
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := NewClient(server.Client(), server.URL, "").ListRepos(ctx, Request(&pb.ListReposRequest{})); err == nil {
		t.Fatal("unauthenticated request unexpectedly succeeded")
	}
	response, err := NewClient(server.Client(), server.URL, token).ListRepos(ctx, Request(&pb.ListReposRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	if len(response.Msg.Repos) != 1 || response.Msg.Repos[0].Id != "isolated-repo" {
		t.Fatalf("unexpected response: %v", response.Msg)
	}
}
