package chatpicker

import (
	"context"
	"fmt"
	"testing"

	"github.com/recurser/boss/internal/domain/rpc"

	pb "github.com/recurser/bossalib/gen/bossanova/v1"
)

func TestEndpointReadOptionsFollowSetting(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		t.Run(fmt.Sprint(enabled), func(t *testing.T) {
			stub := &endpointReadStub{}
			m := newTestModel(stub, context.Background(), "session-1", "")
			m.SetShowOpenPorts(enabled)
			m.fetchSession()()
			m.refreshStatuses()()
			if len(stub.reads) != 2 {
				t.Fatalf("reads = %v, want both picker reads", stub.reads)
			}
			for _, got := range stub.reads {
				if got != enabled {
					t.Fatalf("endpoint opt-in = %v, want %v", got, enabled)
				}
			}
		})
	}
}

type endpointReadStub struct {
	chatPickerStub
	reads []bool
}

func (s *endpointReadStub) GetSession(_ context.Context, _ string, opts rpc.SessionReadOptions) (*pb.Session, error) {
	s.reads = append(s.reads, opts.IncludeLocalHTTPEndpoints)
	return &pb.Session{}, nil
}
