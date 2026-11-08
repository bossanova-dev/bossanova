package bossmcp

import (
	"context"
	"errors"
	"testing"

	"github.com/recurser/bossalib/kit/projectconfig"
)

func TestPlanLocalProjectConfigWithoutHome(t *testing.T) {
	homeErr := errors.New("home unavailable")
	prev := userHomeDir
	userHomeDir = func() (string, error) { return "", homeErr }
	t.Cleanup(func() { userHomeDir = prev })

	for _, tc := range []struct {
		name    string
		daemon  projectconfig.Daemon
		wantErr bool
	}{
		{name: "matching ID with home-relative path", daemon: projectconfig.Daemon{Name: "laptop", DaemonID: new("d-1"), Path: new("~/src/widget")}, wantErr: true},
		{name: "matching name with home-relative path", daemon: projectconfig.Daemon{Name: "laptop", Path: new("~/src/widget")}, wantErr: true},
		{name: "matching absolute path", daemon: projectconfig.Daemon{Name: "laptop", Path: new(projectCheckout)}},
		{name: "matching entry without path", daemon: projectconfig.Daemon{Name: "laptop"}},
		{name: "unmatched home-relative path", daemon: projectconfig.Daemon{Name: "remote", Path: new("~/src/widget")}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := newProjectLocal()
			cfg := &projectconfig.Config{Project: "widget", Repo: projectconfig.Repo{OriginURL: projectOrigin}, Daemons: []projectconfig.Daemon{tc.daemon}}
			run, err := planLocalProjectConfig(context.Background(), d, cfg, projectCheckout)
			if tc.wantErr {
				if !errors.Is(err, homeErr) {
					t.Fatalf("error = %v, want wrapped home error", err)
				}
				if run != nil {
					t.Fatal("home resolution failure returned a plan")
				}
			} else {
				if err != nil {
					t.Fatal(err)
				}
				if run.local.state.TargetPath != projectCheckout {
					t.Fatalf("target = %q, want %q", run.local.state.TargetPath, projectCheckout)
				}
			}
			if len(d.writes()) != 0 {
				t.Fatalf("planning wrote: %v", d.writes())
			}
		})
	}
}
