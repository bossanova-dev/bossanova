package tmux

import (
	"context"
	"fmt"
	"os/exec"
	"slices"
	"strings"
	"testing"
	"testing/synctest"
	"time"
)

// Bubble both the fake pane frames and draw windows so host scheduling cannot
// skip a frame; polling still spans multiple attempt windows on the fake clock.
func TestTypedDrawKeepsProgressAndRetypesAStalledPrefix(t *testing.T) {
	for _, stalled := range []bool{false, true} {
		t.Run(fmt.Sprintf("stalled=%t", stalled), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				undrawn, drawn, submitted := typedSlashPanes(t)
				prefix := func(text string) string {
					return strings.Replace(undrawn, undrawnPlaceholderRow, "❯ "+text, 1)
				}
				rules := []capturePaneRule{{0, undrawn}, {20 * time.Millisecond, prefix("/boss")}}
				if !stalled {
					rules = append(rules, capturePaneRule{120 * time.Millisecond, prefix("/boss-plan --exclude")})
					rules = append(rules, capturePaneRule{250 * time.Millisecond, drawn})
				}
				fake := &sendPlanRecordingFactory{capturePaneRules: rules}
				typeCount := 0
				c := NewClient(WithCommandFactory(func(ctx context.Context, name string, args ...string) *exec.Cmd {
					if slices.Contains(args, typedSlashPayload) {
						typeCount++
						if stalled && typeCount == 2 {
							// A stalled composer cannot finish before the retry, even if
							// scheduling skips over its entire timeout window.
							fake.capturePaneRules = nil
							fake.capturePaneOutputs = []string{drawn}
						}
					}
					if slices.Contains(args, "Enter") {
						fake.capturePaneRules = nil
						fake.capturePaneOutputs = []string{submitted}
					}
					return fake.factory(ctx, name, args...)
				}))
				if err := c.sendLine(context.Background(), "boss-test-typed", typedSlashPayload, typedDrawOpts(100*time.Millisecond, 3)); err != nil {
					t.Fatalf("sendLine: %v", err)
				}
				firstType := callIndex(fake.calls, "send-keys", typedSlashPayload)
				clear := callIndex(fake.calls, "send-keys", "C-u")
				enter := callIndex(fake.calls, "send-keys", "Enter")
				if stalled {
					if clear <= firstType || callIndex(fake.calls[clear+1:], "send-keys", typedSlashPayload) < 0 || enter <= clear {
						t.Fatal("stalled prefix must be cleared before retyping and submitting")
					}
				} else if firstType < 0 || enter <= firstType || clear >= 0 {
					t.Fatal("progress must be kept from first type through Enter without C-u")
				}
				typed, cleared, entered := 0, 0, 0
				for _, call := range fake.calls {
					if call.subcommand != "send-keys" {
						continue
					}
					switch call.args[len(call.args)-1] {
					case typedSlashPayload:
						typed++
					case "C-u":
						cleared++
					case "Enter":
						entered++
					}
				}
				wantRetypes := 0
				if stalled {
					wantRetypes = 1
				}
				if typed != 1+wantRetypes || cleared != wantRetypes || entered != 1 {
					t.Fatalf("typed=%d cleared=%d Enter=%d; want %d, %d, 1", typed, cleared, entered, 1+wantRetypes, wantRetypes)
				}
			})
		})
	}
}
