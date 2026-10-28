package server

import (
	"testing"

	"connectrpc.com/connect"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/kit/vcs"
	"github.com/recurser/bossd/internal/status"
)

func TestRefreshSessionPRPartialMarker(t *testing.T) {
	tr := status.NewDisplayTracker()
	provider := &refreshPRProvider{prStatus: &vcs.PRStatus{State: vcs.PRStateOpen, Draft: true, HeadSHA: "head", Title: "fix (partial 1/2)"}}
	s := newRefreshPRServer(tr, provider, refreshPRSession())
	for _, partial := range []bool{true, false} {
		if !partial {
			provider.prStatus.Title = "fix"
		}
		if _, err := s.RefreshSessionPR(t.Context(), connect.NewRequest(&pb.RefreshSessionPRRequest{Id: strPtr("s1")})); err != nil {
			t.Fatal(err)
		}
		if got := tr.Get("s1").IsPartial; got != partial {
			t.Fatalf("partial=%v want=%v", got, partial)
		}
	}
}
