package server

import (
	"context"
	"errors"
	"strings"
	"testing"

	"connectrpc.com/connect"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossd/internal/domain"
)

type proofCheckRunPort struct {
	calls int
	token string
	in    ProofCheckRun
	err   error
}

func (p *proofCheckRunPort) UpsertProofCheckRun(_ context.Context, token string, in ProofCheckRun) (ProofCheckRunResult, error) {
	p.calls++
	p.token, p.in = token, in
	return ProofCheckRunResult{HTMLURL: "https://github.com/acme/widgets/runs/1", IsCreated: true}, p.err
}

type proofCheckRunRepos struct {
	repoStore
	repos []*domain.Repo
	err   error
}

func (r proofCheckRunRepos) List(context.Context) ([]*domain.Repo, error) { return r.repos, r.err }

func TestPostProofCheckRun_sessions_AC255801(t *testing.T) {
	for _, tc := range []struct {
		name                                         string
		repo, head, token                            string
		unregistered, needsLogin, noReporter, noPort bool
		upstream, storeErr                           error
		code                                         connect.Code
		reason                                       string
	}{
		{name: "success"},
		{name: "invalid repo", repo: "bad", code: connect.CodeInvalidArgument, reason: "invalid-argument"},
		{name: "invalid head", head: "abcd", code: connect.CodeInvalidArgument, reason: "invalid-argument"},
		{name: "unregistered", unregistered: true, code: connect.CodeFailedPrecondition, reason: "repo-not-registered"},
		{name: "repository read fails", storeErr: errors.New("private database error"), code: connect.CodeUnavailable, reason: "upload-failed"},
		{name: "no token", token: "none", code: connect.CodeFailedPrecondition, reason: "not-signed-in"},
		{name: "needs login", needsLogin: true, code: connect.CodeFailedPrecondition, reason: "not-signed-in"},
		{name: "no auth reporter", noReporter: true, code: connect.CodeFailedPrecondition, reason: "not-signed-in"},
		{name: "no port", noPort: true, code: connect.CodeUnavailable, reason: "upload-failed"},
		{name: "no installation", upstream: connect.NewError(connect.CodePermissionDenied, errors.New("private refusal")), code: connect.CodePermissionDenied, reason: "no-installation"},
		{name: "checks not granted", upstream: connect.NewError(connect.CodeFailedPrecondition, errors.New("checks write not granted")), code: connect.CodeFailedPrecondition, reason: "checks-not-granted"},
		{name: "other precondition", upstream: connect.NewError(connect.CodeFailedPrecondition, errors.New("other")), code: connect.CodeUnavailable, reason: "upload-failed"},
		{name: "upstream failure", upstream: errors.New("private upstream error"), code: connect.CodeUnavailable, reason: "upload-failed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			port := &proofCheckRunPort{err: tc.upstream}
			repos := []*domain.Repo{{OriginURL: "git@github.com:acme/widgets.git"}}
			if tc.unregistered {
				repos = []*domain.Repo{{OriginURL: "https://github.com/other/widgets"}}
			}
			s := &Server{repos: proofCheckRunRepos{repos: repos, err: tc.storeErr}, proofCheckRuns: port, proofCheckRunSessionToken: func() string {
				if tc.token == "none" {
					return ""
				}
				return "daemon-token"
			}, authStateReporter: &stubAuthStateReporter{state: DaemonAuthState{NeedsLogin: tc.needsLogin}}}
			if tc.noReporter {
				s.authStateReporter = nil
			}
			if tc.noPort {
				s.proofCheckRuns = nil
			}
			req := &pb.PostProofCheckRunRequest{Repo: "acme/widgets", HeadSha: strings.Repeat("a", 40), Title: "Proof", Summary: "Summary", DetailsUrl: "https://proof.example/run"}
			if tc.repo != "" {
				req.Repo = tc.repo
			}
			if tc.head != "" {
				req.HeadSha = tc.head
			}
			resp, err := s.PostProofCheckRun(context.Background(), connect.NewRequest(req))
			if tc.code != 0 {
				var rpcErr *connect.Error
				if !errors.As(err, &rpcErr) || rpcErr.Code() != tc.code || rpcErr.Meta().Get(ProofCheckReasonHeader) != tc.reason {
					t.Fatalf("error = %v; want %v / %s", err, tc.code, tc.reason)
				}
				wantCalls := 0
				if tc.upstream != nil {
					wantCalls = 1
				}
				if port.calls != wantCalls {
					t.Fatalf("calls = %d, want %d", port.calls, wantCalls)
				}
				if strings.Contains(err.Error(), "private") {
					t.Fatalf("leaked internal error: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if port.calls != 1 || port.token != "daemon-token" || port.in != (ProofCheckRun{RepoOriginURL: "https://github.com/acme/widgets", HeadSHA: req.HeadSha, Title: req.Title, Summary: req.Summary, DetailsURL: req.DetailsUrl}) {
				t.Fatalf("forwarded = %+v", port)
			}
			if resp.Msg.HtmlUrl != "https://github.com/acme/widgets/runs/1" || !resp.Msg.IsCreated {
				t.Fatalf("response = %v", resp.Msg)
			}
		})
	}
}
