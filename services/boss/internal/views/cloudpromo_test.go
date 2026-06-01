package views

import (
	"context"
	"strings"
	"testing"
)

func TestCloudDiscoveryLine(t *testing.T) {
	line := cloudDiscoveryLine(false, true)
	for _, want := range []string{
		"Bossanova Cloud",
		"[l]ogin to try Bossanova Cloud for free",
		"realtime GitHub events",
		"web remote control",
	} {
		if !strings.Contains(line, want) {
			t.Fatalf("cloudDiscoveryLine(false, true) = %q, want %q", line, want)
		}
	}

	if got := cloudDiscoveryLine(true, true); got != "" {
		t.Fatalf("cloudDiscoveryLine(true, true) = %q, want empty", got)
	}

	if got := cloudDiscoveryLine(false, false); got != "" {
		t.Fatalf("cloudDiscoveryLine(false, false) = %q, want empty", got)
	}
}

func TestCloudSettingsBlock(t *testing.T) {
	block := cloudSettingsBlock()
	for _, want := range []string{
		"Bossanova Cloud",
		"7-day free trial",
		"press [l]ogin from Home",
		"local mode stays free",
		"optional",
	} {
		if !strings.Contains(block, want) {
			t.Fatalf("cloudSettingsBlock() = %q, want %q", block, want)
		}
	}
}

func TestSettingsViewHidesCloudBlockWithoutAuth(t *testing.T) {
	m := NewSettingsModel(nil, context.Background())
	view := m.View().Content
	if strings.Contains(view, "press [l]ogin from Home") {
		t.Fatalf("settings view showed cloud login prompt without auth configured: %q", view)
	}
}
