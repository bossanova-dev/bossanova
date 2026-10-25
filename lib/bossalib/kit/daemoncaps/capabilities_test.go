package daemoncaps

import (
	"context"
	"errors"
	"strings"
	"testing"

	"connectrpc.com/connect"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
)

type fakeProber struct {
	capabilities []string
	err          error
}

func (f fakeProber) GetDaemonCapabilities(context.Context, *connect.Request[pb.GetDaemonCapabilitiesRequest]) (*connect.Response[pb.GetDaemonCapabilitiesResponse], error) {
	if f.err != nil {
		return nil, f.err
	}
	return connect.NewResponse(&pb.GetDaemonCapabilitiesResponse{Capabilities: f.capabilities}), nil
}

func TestRequire(t *testing.T) {
	unavailable := connect.NewError(connect.CodeUnavailable, errors.New("socket unavailable"))
	tests := []struct {
		name        string
		probe       fakeProber
		want        connect.Code
		unsupported bool
	}{
		{name: "listed", probe: fakeProber{capabilities: []string{"other", MergeExpectedHeadSHA}}},
		{name: "not listed", probe: fakeProber{capabilities: []string{"other"}}, want: connect.CodeUnimplemented, unsupported: true},
		{name: "legacy", probe: fakeProber{err: connect.NewError(connect.CodeUnimplemented, errors.New("unknown method"))}, want: connect.CodeUnimplemented, unsupported: true},
		{name: "transport", probe: fakeProber{err: unavailable}, want: connect.CodeUnavailable},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := Require(context.Background(), tt.probe, MergeExpectedHeadSHA)
			if tt.want == 0 {
				if err != nil {
					t.Fatalf("Require: %v", err)
				}
				return
			}
			if connect.CodeOf(err) != tt.want {
				t.Fatalf("code = %v, want %v (err=%v)", connect.CodeOf(err), tt.want, err)
			}
			if tt.unsupported {
				for _, part := range []string{"bossd", "merge head pin", "not attempted", "restart", "upgrade"} {
					if !strings.Contains(err.Error(), part) {
						t.Errorf("error %q missing remedy/operation fact %q", err, part)
					}
				}
			} else if !errors.Is(err, unavailable) {
				t.Fatalf("transport cause lost: %v", err)
			}
		})
	}
}
