package main

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	server "github.com/recurser/bossd/api/daemon"
	"google.golang.org/protobuf/types/known/timestamppb"
)

type hostedProofViewUpstreamFake struct {
	calls int
	auth  string
	msg   *pb.MintProofViewLinkRequest
	resp  *pb.MintProofViewLinkResponse
	err   error
}

func (f *hostedProofViewUpstreamFake) MintProofViewLink(_ context.Context, req *connect.Request[pb.MintProofViewLinkRequest]) (*connect.Response[pb.MintProofViewLinkResponse], error) {
	f.calls++
	f.auth, f.msg = req.Header().Get("Authorization"), req.Msg
	if f.err != nil {
		return nil, f.err
	}
	return connect.NewResponse(f.resp), nil
}

func TestHostedProofViewAdapter(t *testing.T) {
	expires := time.Date(2026, 11, 25, 12, 0, 0, 0, time.UTC)
	target := server.HostedProofViewTarget{RepoOriginURL: "https://github.com/acme/widgets", PRNumber: 7, HeadSHA: strings.Repeat("a", 40)}
	t.Run("sends the user token and the mapped target", func(t *testing.T) {
		up := &hostedProofViewUpstreamFake{resp: &pb.MintProofViewLinkResponse{ViewUrl: "https://acme.proofs.test/v1.1.sig/tok/", ExpiresAt: timestamppb.New(expires)}}
		link, err := hostedProofViewClient{rpc: up, userToken: func() string { return "user-jwt" }}.MintProofViewLink(t.Context(), target)
		if err != nil {
			t.Fatal(err)
		}
		if up.auth != "Bearer user-jwt" {
			t.Fatalf("Authorization = %q", up.auth)
		}
		if up.msg.GetRepoOriginUrl() != target.RepoOriginURL || up.msg.GetPrNumber() != target.PRNumber || up.msg.GetHeadSha() != target.HeadSHA {
			t.Fatalf("request = %v", up.msg)
		}
		if link.ViewURL != "https://acme.proofs.test/v1.1.sig/tok/" || !link.ExpiresAt.Equal(expires) {
			t.Fatalf("link = %+v", link)
		}
	})
	t.Run("empty token is signed out and sends nothing", func(t *testing.T) {
		up := &hostedProofViewUpstreamFake{}
		_, err := hostedProofViewClient{rpc: up, userToken: func() string { return "" }}.MintProofViewLink(t.Context(), target)
		if !errors.Is(err, server.ErrHostedProofViewSignedOut) || up.calls != 0 {
			t.Fatalf("err = %v, calls = %d", err, up.calls)
		}
	})
	t.Run("orchestrator error passes through", func(t *testing.T) {
		up := &hostedProofViewUpstreamFake{err: connect.NewError(connect.CodeNotFound, errors.New("proof page not found"))}
		_, err := hostedProofViewClient{rpc: up, userToken: func() string { return "user-jwt" }}.MintProofViewLink(t.Context(), target)
		if connect.CodeOf(err) != connect.CodeNotFound {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("falls back to the fixed user token", func(t *testing.T) {
		if got := userTokenOr(func() string { return "" }, "env-jwt")(); got != "env-jwt" {
			t.Fatalf("fallback token = %q", got)
		}
		if got := userTokenOr(func() string { return "keychain-jwt" }, "env-jwt")(); got != "keychain-jwt" {
			t.Fatalf("provider token = %q", got)
		}
	})
}
