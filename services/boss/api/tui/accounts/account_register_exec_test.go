package accounts

import (
	"context"
	"testing"
	"time"
)

func TestStartFlowWiresHandoffOnlyWhereItSpawns(t *testing.T) {
	cases := []struct {
		name     string
		provider string
		remote   string
		want     bool
	}{
		{"local claude spawns -> bridged", "claude", "", true},
		{"codex must not be bridged", "codex", "", false},
		{"remote claude pastes, spawns nothing", "claude", "ssh-dest", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := NewAgentAccountRegisterModel(&regAcctClient{}, context.Background())
			m.remoteHost = tc.remote
			got, _ := m.startFlow(tc.provider)
			// startFlow spawns the real flow goroutine, and that goroutine reads
			// package-level --host state on its way to the first prompt
			// (isRemoteHost, via the prompter's hostAwareFlowText). Cancelling it
			// is not enough: an abandoned goroutine is unordered against the next
			// test's SetHostDestination, which -race reports as a data race even
			// once the goroutine has finished. Join it, as the flow tests in
			// account_register_test.go do, so the read is ordered before the write.
			t.Cleanup(func() {
				got.teardown()
				select {
				case <-got.flowDone:
				case <-time.After(3 * time.Second):
					t.Error("flow goroutine leaked after teardown")
				}
			})

			if (got.execBridge != nil) != tc.want {
				t.Fatalf("execBridge non-nil = %v, want %v", got.execBridge != nil, tc.want)
			}
		})
	}
}
