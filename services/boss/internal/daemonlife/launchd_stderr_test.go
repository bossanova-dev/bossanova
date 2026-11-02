//go:build darwin

package daemonlife

import (
	"bytes"
	"encoding/xml"
	"path/filepath"
	"strings"
	"testing"

	"github.com/recurser/bossalib/kit/config"
)

// plistStringAfterKey returns the <string> that follows the first <key>want</key>.
func plistStringAfterKey(t *testing.T, plist, want string) string {
	t.Helper()
	decoder := xml.NewDecoder(strings.NewReader(plist))
	found := false
	for {
		token, err := decoder.Token()
		if err != nil {
			t.Fatalf("plist has no <string> after key %q: %v", want, err)
		}
		start, ok := token.(xml.StartElement)
		if !ok || (start.Name.Local != "key" && start.Name.Local != "string") {
			continue
		}
		var text string
		if err := decoder.DecodeElement(&text, &start); err != nil {
			t.Fatalf("decode %s: %v", start.Name.Local, err)
		}
		switch {
		case start.Name.Local == "key" && text == want:
			found = true
		case found && start.Name.Local == "string":
			return text
		}
	}
}

// TestGeneratedPlistDeclaresItsStderrPathToBossd_DaemonLifecycle_AC217102: bossd treats itself as
// launchd-supervised only when BOSSD_STDERR_LOG_PATH names the file launchd
// redirects fd 2 into, so the two values must be the same string.
func TestGeneratedPlistDeclaresItsStderrPathToBossd_DaemonLifecycle_AC217102(t *testing.T) {
	stubHome(t)
	stubServiceSettings(t, config.Settings{})
	t.Setenv(ServiceSettingsPathEnv, "")

	plist, err := generatePlist("/usr/local/bin/bossd")
	if err != nil {
		t.Fatalf("generatePlist: %v", err)
	}

	stderrPath := plistStringAfterKey(t, plist, "StandardErrorPath")
	if filepath.Base(stderrPath) != "bossd.stderr.log" || !filepath.IsAbs(stderrPath) {
		t.Fatalf("StandardErrorPath = %q, want an absolute bossd.stderr.log path", stderrPath)
	}
	declared, ok := plistEnvironmentValue([]byte(plist), "BOSSD_STDERR_LOG_PATH")
	if !ok {
		t.Fatalf("plist EnvironmentVariables has no BOSSD_STDERR_LOG_PATH:\n%s", plist)
	}
	if declared != stderrPath {
		t.Errorf("BOSSD_STDERR_LOG_PATH = %q, StandardErrorPath = %q; bossd would never see itself as supervised", declared, stderrPath)
	}
}

// TestGeneratedPlistStderrDeclarationKeepsSettingsPathRendering pins that the
// new key leaves the BOS-1368 settings-path entry intact and the plist valid.
func TestGeneratedPlistStderrDeclarationKeepsSettingsPathRendering(t *testing.T) {
	stubHome(t)
	stubServiceSettings(t, config.Settings{})
	t.Setenv(ServiceSettingsPathEnv, "/abs/x/settings.json")

	plist, err := generatePlist("/usr/local/bin/bossd")
	if err != nil {
		t.Fatalf("generatePlist: %v", err)
	}
	if got, ok := plistSettingsPath([]byte(plist)); !ok || got != "/abs/x/settings.json" {
		t.Errorf("settings path reads back as (%q, %v), want the baked path", got, ok)
	}
	if got, ok := plistEnvironmentValue([]byte(plist), "BOSSD_STDERR_LOG_PATH"); !ok || got != plistStringAfterKey(t, plist, "StandardErrorPath") {
		t.Errorf("BOSSD_STDERR_LOG_PATH reads back as (%q, %v), want the StandardErrorPath", got, ok)
	}
	var v struct{}
	if err := xml.NewDecoder(bytes.NewReader([]byte(plist))).Decode(&v); err != nil {
		t.Errorf("plist is not well-formed XML: %v", err)
	}
}
