package session

import (
	"testing"

	"github.com/spf13/cobra"
)

func TestTUILaunchReceivesInitialDestination(t *testing.T) {
	for _, tt := range []struct {
		name string
		args []string
		want LaunchOptions
	}{
		{name: "new", args: []string{"--agent", "codex", "--account="}, want: LaunchOptions{NewSession: true, Agent: "codex", Account: ptrTo("")}},
		{name: "attach", args: []string{"session-123"}, want: LaunchOptions{AttachSessionID: "session-123"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			calls := 0
			g := New(func(*cobra.Command) (Client, error) {
				t.Fatal("interactive launch must resolve its own client")
				return nil, nil
			}, Options{LaunchTUI: func(_ *cobra.Command, got LaunchOptions) error {
				calls++
				if got.NewSession != tt.want.NewSession || got.Agent != tt.want.Agent || got.AttachSessionID != tt.want.AttachSessionID {
					t.Fatalf("launch options = %#v, want %#v", got, tt.want)
				}
				if (got.Account == nil) != (tt.want.Account == nil) || got.Account != nil && *got.Account != *tt.want.Account {
					t.Fatalf("account presence/value = %v, want %v", got.Account, tt.want.Account)
				}
				return nil
			}})
			cmd := g.NewCmd()
			if tt.name == "attach" {
				cmd = g.AttachCmd()
			}
			cmd.SetArgs(tt.args)
			if err := cmd.Execute(); err != nil {
				t.Fatal(err)
			}
			if calls != 1 {
				t.Fatalf("launch calls = %d", calls)
			}
		})
	}
}
