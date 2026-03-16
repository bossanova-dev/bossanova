package daemon

import (
	"strings"
	"testing"
)

func TestGeneratePlist(t *testing.T) {
	plist, err := GeneratePlist("/usr/local/bin/bossd")
	if err != nil {
		t.Fatalf("GeneratePlist: %v", err)
	}

	checks := []string{
		"<string>com.bossanova.bossd</string>",
		"<string>/usr/local/bin/bossd</string>",
		"<key>RunAtLoad</key>",
		"<true/>",
		"<key>KeepAlive</key>",
		"bossd.stdout.log",
		"bossd.stderr.log",
	}

	for _, check := range checks {
		if !strings.Contains(plist, check) {
			t.Errorf("plist missing %q", check)
		}
	}
}

func TestPlistPath(t *testing.T) {
	path, err := PlistPath()
	if err != nil {
		t.Fatalf("PlistPath: %v", err)
	}

	if !strings.HasSuffix(path, "Library/LaunchAgents/com.bossanova.bossd.plist") {
		t.Errorf("unexpected plist path: %s", path)
	}
}
