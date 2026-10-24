package home

import (
	"context"
	"fmt"
	"testing"
)

func TestHomeEndpointReadOptionsFollowSetting(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		t.Run(fmt.Sprint(enabled), func(t *testing.T) {
			stub := &endpointReadStub{stubClient: &stubClient{}}
			FetchSessions(stub, context.Background(), 0, 0, enabled)()
			if len(stub.reads) != 1 || stub.reads[0] != enabled {
				t.Fatalf("reads = %v, want %v", stub.reads, enabled)
			}
		})
	}
}

type endpointReadStub struct {
	*stubClient
	reads []bool
}
