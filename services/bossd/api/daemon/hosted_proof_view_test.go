package server

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
)

type hostedProofViewsFake struct {
	calls int
	in    HostedProofViewTarget
	link  HostedProofViewLink
	err   error
}

func (f *hostedProofViewsFake) MintProofViewLink(_ context.Context, in HostedProofViewTarget) (HostedProofViewLink, error) {
	f.calls++
	f.in = in
	return f.link, f.err
}

const hostedProofOrigin = "https://github.com/acme/widgets"

func TestMintHostedProofViewLink(t *testing.T) {
	expires := time.Date(2026, 11, 25, 12, 0, 0, 0, time.UTC)
	head := strings.Repeat("a", 40)
	for _, tc := range []struct {
		name       string
		req        *pb.MintHostedProofViewLinkRequest
		noClient   bool
		needsLogin bool
		upstream   error
		code       connect.Code
		wantCalls  int
	}{
		{name: "forwards the target", req: &pb.MintHostedProofViewLinkRequest{RepoOriginUrl: hostedProofOrigin, PrNumber: 7, HeadSha: head}, wantCalls: 1},
		{name: "empty head means newest upload", req: &pb.MintHostedProofViewLinkRequest{RepoOriginUrl: hostedProofOrigin, PrNumber: 7}, wantCalls: 1},
		{name: "adapter has no user token", req: &pb.MintHostedProofViewLinkRequest{RepoOriginUrl: hostedProofOrigin, PrNumber: 7}, upstream: ErrHostedProofViewSignedOut, code: connect.CodeFailedPrecondition, wantCalls: 1},
		{name: "needs login", req: &pb.MintHostedProofViewLinkRequest{RepoOriginUrl: hostedProofOrigin, PrNumber: 7}, needsLogin: true, code: connect.CodeFailedPrecondition},
		{name: "local-only daemon", req: &pb.MintHostedProofViewLinkRequest{RepoOriginUrl: hostedProofOrigin, PrNumber: 7}, noClient: true, code: connect.CodeFailedPrecondition},
		{name: "session origin from a .git/ remote forwards canonically", req: &pb.MintHostedProofViewLinkRequest{RepoOriginUrl: hostedProofOrigin + ".git", PrNumber: 7}, wantCalls: 1},
		{name: ".git/ origin forwards canonically", req: &pb.MintHostedProofViewLinkRequest{RepoOriginUrl: hostedProofOrigin + ".git/", PrNumber: 7}, wantCalls: 1},
		{name: "ssh origin forwards canonically", req: &pb.MintHostedProofViewLinkRequest{RepoOriginUrl: "git@github.com:acme/widgets.git", PrNumber: 7}, wantCalls: 1},
		{name: "non-URL origin", req: &pb.MintHostedProofViewLinkRequest{RepoOriginUrl: "not a repository", PrNumber: 7}, code: connect.CodeInvalidArgument},
		{name: "origin with no canonical form", req: &pb.MintHostedProofViewLinkRequest{RepoOriginUrl: "https://github.com/acme/.git", PrNumber: 7}, code: connect.CodeInvalidArgument},
		{name: "empty origin", req: &pb.MintHostedProofViewLinkRequest{PrNumber: 7}, code: connect.CodeInvalidArgument},
		{name: "non-positive PR", req: &pb.MintHostedProofViewLinkRequest{RepoOriginUrl: hostedProofOrigin}, code: connect.CodeInvalidArgument},
		{name: "non-hex head", req: &pb.MintHostedProofViewLinkRequest{RepoOriginUrl: hostedProofOrigin, PrNumber: 7, HeadSha: "not-a-sha"}, code: connect.CodeInvalidArgument},
		{name: "orchestrator NotFound passes through", req: &pb.MintHostedProofViewLinkRequest{RepoOriginUrl: hostedProofOrigin, PrNumber: 7}, upstream: connect.NewError(connect.CodeNotFound, errors.New("proof page not found")), code: connect.CodeNotFound, wantCalls: 1},
		{name: "orchestrator Unavailable passes through", req: &pb.MintHostedProofViewLinkRequest{RepoOriginUrl: hostedProofOrigin, PrNumber: 7}, upstream: connect.NewError(connect.CodeUnavailable, errors.New("proof view links are not configured")), code: connect.CodeUnavailable, wantCalls: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fake := &hostedProofViewsFake{link: HostedProofViewLink{ViewURL: "https://acme.proofs.test/v1.1.sig/tok/", ExpiresAt: expires}, err: tc.upstream}
			cfg := Config{AuthStateReporter: &stubAuthStateReporter{state: DaemonAuthState{NeedsLogin: tc.needsLogin}}}
			if !tc.noClient {
				cfg.HostedProofViews = fake
			}
			resp, err := New(cfg).MintHostedProofViewLink(t.Context(), connect.NewRequest(tc.req))
			if fake.calls != tc.wantCalls {
				t.Fatalf("adapter calls = %d, want %d", fake.calls, tc.wantCalls)
			}
			if tc.code != 0 {
				if connect.CodeOf(err) != tc.code {
					t.Fatalf("code = %v (%v), want %v", connect.CodeOf(err), err, tc.code)
				}
				if tc.code == connect.CodeFailedPrecondition && !strings.Contains(err.Error(), "boss login") {
					t.Fatalf("refusal %q does not tell the user to sign in with boss login", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if want := (HostedProofViewTarget{RepoOriginURL: hostedProofOrigin, PRNumber: 7, HeadSHA: tc.req.GetHeadSha()}); fake.in != want {
				t.Fatalf("target = %+v, want %+v", fake.in, want)
			}
			if resp.Msg.GetViewUrl() != fake.link.ViewURL || !resp.Msg.GetExpiresAt().AsTime().Equal(expires) {
				t.Fatalf("response = %v", resp.Msg)
			}
		})
	}
}
