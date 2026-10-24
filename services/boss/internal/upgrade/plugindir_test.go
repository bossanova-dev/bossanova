package upgrade

import (
	"testing"

	"github.com/recurser/bossalib/contract/daemon/daemonbin"
)

func TestHomebrewPrefixesForBinDirDelegatesToDaemonbin(t *testing.T) {
	for _, binDir := range []string{
		"/opt/homebrew/Cellar/bossanova/2.1.197/bin",
		"/usr/local/Cellar/bossanova/v1.2.3/bin",
		"/usr/local/bin",
		"/opt/homebrew/Cellar/other/1.0.0/bin",
	} {
		gotFormula, gotBrew, gotOK := homebrewPrefixesForBinDir(binDir)
		wantFormula, wantBrew, wantOK := daemonbin.HomebrewCellarPrefixes(binDir)
		if gotFormula != wantFormula || gotBrew != wantBrew || gotOK != wantOK {
			t.Errorf("homebrewPrefixesForBinDir(%q) = (%q, %q, %t), want (%q, %q, %t)",
				binDir, gotFormula, gotBrew, gotOK, wantFormula, wantBrew, wantOK)
		}
	}

	formula, brew, ok := homebrewPrefixesForBinDir("/opt/homebrew/Cellar/bossanova/2.1.197/bin")
	if !ok || formula != "/opt/homebrew/Cellar/bossanova/2.1.197" || brew != "/opt/homebrew" {
		t.Fatalf("homebrewPrefixesForBinDir() = (%q, %q, %t), want the formula and brew prefixes", formula, brew, ok)
	}
}
