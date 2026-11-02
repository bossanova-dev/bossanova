package plugin

import (
	"context"
	"errors"
	"fmt"
	"net"
	"reflect"
	"sync"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	grpcstatus "google.golang.org/grpc/status"

	sharedplugin "github.com/recurser/bossalib/contract/plugin/plugin"
	bossanovav1 "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/kit/vcs"

	"github.com/recurser/bossd/internal/ghusage"
	lifecycleplugin "github.com/recurser/bossd/internal/plugin"
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
		"ListClosedPRs": func(s *HostServiceServer) error {
			_, err := s.ListClosedPRs(context.Background(), &bossanovav1.ListClosedPRsRequest{RepoOriginUrl: "o/r"})
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

// cachedReadsVCSProvider records, per plugin PR read, whether the provider was
// reached with vcs.CachedReadsAllowed(ctx).
type cachedReadsVCSProvider struct {
	mockVCSProvider
	cached map[string]bool
}

func (p *cachedReadsVCSProvider) GetCheckResults(ctx context.Context, repo string, n int) ([]vcs.CheckResult, error) {
	p.cached["GetCheckResults"] = vcs.CachedReadsAllowed(ctx)
	return p.mockVCSProvider.GetCheckResults(ctx, repo, n)
}

func (p *cachedReadsVCSProvider) GetPRStatus(ctx context.Context, repo string, n int) (*vcs.PRStatus, error) {
	p.cached["GetPRStatus"] = vcs.CachedReadsAllowed(ctx)
	return p.mockVCSProvider.GetPRStatus(ctx, repo, n)
}

func (p *cachedReadsVCSProvider) ListClosedPRs(ctx context.Context, repo string) ([]vcs.PRSummary, error) {
	p.cached["ListClosedPRs"] = vcs.CachedReadsAllowed(ctx)
	return p.mockVCSProvider.ListClosedPRs(ctx, repo)
}

// TestHostPluginPRReadsAreBackgroundReads pins that the dependabot plugin's
// per-PR and closed-list reads reach the provider marked as cached background
// reads, so they project from the open-PR snapshot and stop at the background
// quota floor instead of paying GitHub on every poll.
func TestHostPluginPRReadsAreBackgroundReads(t *testing.T) {
	provider := &cachedReadsVCSProvider{
		mockVCSProvider: mockVCSProvider{prStatus: &vcs.PRStatus{}},
		cached:          map[string]bool{},
	}
	srv := newTestHostServiceServer(provider)
	ctx := t.Context()

	if _, err := srv.GetCheckResults(ctx, &bossanovav1.GetCheckResultsRequest{RepoOriginUrl: "o/r", PrNumber: 7}); err != nil {
		t.Fatalf("GetCheckResults: %v", err)
	}
	if _, err := srv.GetPRStatus(ctx, &bossanovav1.GetPRStatusRequest{RepoOriginUrl: "o/r", PrNumber: 7}); err != nil {
		t.Fatalf("GetPRStatus: %v", err)
	}
	if _, err := srv.ListClosedPRs(ctx, &bossanovav1.ListClosedPRsRequest{RepoOriginUrl: "o/r"}); err != nil {
		t.Fatalf("ListClosedPRs: %v", err)
	}
	for _, read := range []string{"GetCheckResults", "GetPRStatus", "ListClosedPRs"} {
		cached, reached := provider.cached[read]
		if !reached {
			t.Errorf("%s never reached the provider", read)
			continue
		}
		if !cached {
			t.Errorf("%s reached the provider without vcs.WithCachedReads", read)
		}
	}
}

// labelRecordingVCSProvider records the usage label each GetReviewComments
// call reaches the provider with.
type labelRecordingVCSProvider struct {
	mockVCSProvider
	mu     sync.Mutex
	labels []string
}

func (p *labelRecordingVCSProvider) GetReviewComments(ctx context.Context, repo string, n int) ([]vcs.ReviewComment, error) {
	p.mu.Lock()
	p.labels = append(p.labels, ghusage.LabelFrom(ctx))
	p.mu.Unlock()
	return p.mockVCSProvider.GetReviewComments(ctx, repo, n)
}

// TestRepairPluginReviewCommentsCarryItsUsageLabel pins plugin attribution
// end to end: a GetReviewComments call arriving over gRPC through the host
// service registered for plugin "repair" reaches the provider labelled
// plugin:repair.
func TestRepairPluginReviewCommentsCarryItsUsageLabel(t *testing.T) {
	provider := &labelRecordingVCSProvider{}
	plugins := lifecycleplugin.NewVersionedPluginMap(newTestHostServiceServer(provider), "repair")
	agentRunner, ok := plugins[sharedplugin.ProtocolVersion][sharedplugin.PluginTypeAgentRunner].(*sharedplugin.AgentRunnerPlugin)
	if !ok {
		t.Fatal("the versioned plugin map has no agent runner plugin")
	}
	host := agentRunner.HostService
	var opts []grpc.ServerOption
	if optioner, ok := host.(sharedplugin.HostServerOptioner); ok {
		opts = optioner.HostServerOptions()
	}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen tcp: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	gs := grpc.NewServer(opts...)
	host.Register(gs)
	go func() { _ = gs.Serve(ln) }()
	t.Cleanup(gs.Stop)
	conn, err := grpc.NewClient(ln.Addr().String(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("grpc dial: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })

	resp := new(bossanovav1.GetReviewCommentsResponse)
	if err := conn.Invoke(t.Context(), bossanovav1.HostService_GetReviewComments_FullMethodName,
		&bossanovav1.GetReviewCommentsRequest{RepoOriginUrl: "o/r", PrNumber: 7}, resp); err != nil {
		t.Fatalf("GetReviewComments: %v", err)
	}
	provider.mu.Lock()
	defer provider.mu.Unlock()
	if want := []string{ghusage.PluginLabel("repair")}; !reflect.DeepEqual(provider.labels, want) {
		t.Fatalf("provider labels = %v, want %v", provider.labels, want)
	}
}
