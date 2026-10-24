package skills

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	daemonrpc "github.com/recurser/boss/internal/adapter/daemonrpc"
	"github.com/spf13/cobra"
)

func newTestGroup() *Group {
	g := New(func(cmd *cobra.Command) (Client, error) {
		socket, err := daemonrpc.DefaultSocketPath()
		if err != nil {
			return nil, err
		}
		return daemonrpc.NewLocal(socket), nil
	}, Config{ShutdownHostTunnel: func() {}, LoadProject: func(context.Context, string) (*Project, error) { return nil, nil }})
	g.InitIsTerminal = func() bool { return false }
	return g
}

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "boss-skills-settings-*")
	if err != nil {
		panic(err)
	}
	if err := os.Setenv("BOSS_SETTINGS_PATH", filepath.Join(dir, "settings.json")); err != nil {
		panic(err)
	}
	code := m.Run()
	_ = os.RemoveAll(dir)
	os.Exit(code)
}
