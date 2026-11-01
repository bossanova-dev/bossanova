package plugin

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"google.golang.org/grpc/codes"
	grpcstatus "google.golang.org/grpc/status"

	bossanovav1 "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/kit/vcs"
)

// reviewErrProvider is mockVCSProvider with GetReviewComments failing too.
type reviewErrProvider struct{ *mockVCSProvider }

func (p reviewErrProvider) GetReviewComments(context.Context, string, int) ([]vcs.ReviewComment, error) {
	return nil, p.err
}

// TestHostGitHubReadsReportQuotaRefusalAsResourceExhausted pins that plugins
// see a provider read refused for an exhausted quota as ResourceExhausted,
// and every other provider error unchanged.
func TestHostGitHubReadsReportQuotaRefusalAsResourceExhausted(t *testing.T) {
	reads := map[string]func(*HostServiceServer) error{
		"ListOpenPRs": func(s *HostServiceServer) error {
			_, err := s.ListOpenPRs(context.Background(), &bossanovav1.ListOpenPRsRequest{RepoOriginUrl: "o/r"})
			return err
		},
		"GetCheckResults": func(s *HostServiceServer) error {
			_, err := s.GetCheckResults(context.Background(), &bossanovav1.GetCheckResultsRequest{RepoOriginUrl: "o/r", PrNumber: 7})
			return err
		},
		"GetPRStatus": func(s *HostServiceServer) error {
			_, err := s.GetPRStatus(context.Background(), &bossanovav1.GetPRStatusRequest{RepoOriginUrl: "o/r", PrNumber: 7})
			return err
		},
		"GetReviewComments": func(s *HostServiceServer) error {
			_, err := s.GetReviewComments(context.Background(), &bossanovav1.GetReviewCommentsRequest{RepoOriginUrl: "o/r", PrNumber: 7})
			return err
		},
	}
	refused := fmt.Errorf("get PR: %w", vcs.ErrRateLimited)
	other := errors.New("gh: network unreachable")
	for name, read := range reads {
		t.Run(name, func(t *testing.T) {
			err := read(newTestHostServiceServer(reviewErrProvider{&mockVCSProvider{err: refused}}))
			if got := grpcstatus.Code(err); got != codes.ResourceExhausted {
				t.Fatalf("refused read code = %v (err %v), want ResourceExhausted", got, err)
			}
			if !vcs.IsRateLimited(err) {
				t.Fatalf("vcs.IsRateLimited(%v) = false", err)
			}
			if err := read(newTestHostServiceServer(reviewErrProvider{&mockVCSProvider{err: other}})); !errors.Is(err, other) {
				t.Fatalf("other read err = %v, want the provider error unchanged", err)
			}
		})
	}
}
