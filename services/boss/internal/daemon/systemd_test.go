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
		// BOS-457: raise the FD limit so setup scripts bossd spawns don't die
		// with EMFILE during FD-heavy steps like prisma codegen.
		"LimitNOFILE=65536",
	}

	for _, check := range checks {
		if !strings.Contains(unit, check) {
			t.Errorf("unit file missing %q", check)
		}
	}

	// BOS-457: the FD-limit raise is scoped to bossd only; the MCP server does
	// not spawn FD-hungry setup scripts, so its unit must not carry the key.
	mcpUnit, err := generateMcpUnit("/usr/local/bin/mcp", 8765)
	if err != nil {
		t.Fatalf("generateMcpUnit: %v", err)
	}
	if strings.Contains(mcpUnit, "LimitNOFILE") {
		t.Error("MCP unit should not contain LimitNOFILE (bossd-only FD raise)")
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
