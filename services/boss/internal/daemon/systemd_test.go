//go:build linux

package daemon

import (
	"strings"
	"testing"
)

func TestGenerateUnit(t *testing.T) {
	unit, err := generateUnit("/usr/local/bin/bossd")
	if err != nil {
		t.Fatalf("generateUnit: %v", err)
	}

	checks := []string{
		"Description=Bossanova Daemon",
		"ExecStart=/usr/local/bin/bossd",
		"Restart=always",
		"RestartSec=5",
		"WantedBy=default.target",
	}

	for _, check := range checks {
		if !strings.Contains(unit, check) {
			t.Errorf("unit file missing %q", check)
		}
	}
}

func TestSystemdServicePath(t *testing.T) {
	path, err := platformServicePath()
	if err != nil {
		t.Fatalf("platformServicePath: %v", err)
	}

	if !strings.HasSuffix(path, ".config/systemd/user/bossd.service") {
		t.Errorf("unexpected service path: %s", path)
	}
}
