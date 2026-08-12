package apiversion_test

import (
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"testing"

	"github.com/recurser/bossalib/apiversion"
)

// webAPIVersionRE extracts the API_VERSION constant the web client stamps onto
// every request (services/web/src/api.ts). The web codebase cannot import the
// Go registry, so this guard ties the hardcoded TypeScript constant back to the
// Go source of truth: if a future Baseline/registry change leaves the web value
// unsupported, every web request would be rejected with CodeInvalidArgument.
var webAPIVersionRE = regexp.MustCompile(`API_VERSION\s*=\s*'([0-9]{4}-[0-9]{2}-[0-9]{2})'`)

// readWebAPIVersion extracts the API_VERSION constant out of
// services/web/src/api.ts, failing the test if it cannot be found.
func readWebAPIVersion(t *testing.T) apiversion.Version {
	t.Helper()
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	// thisFile = <repo>/lib/bossalib/apiversion/webversion_test.go
	repoRoot := filepath.Join(filepath.Dir(thisFile), "..", "..", "..")
	apiTS := filepath.Join(repoRoot, "services", "web", "src", "api.ts")

	data, err := os.ReadFile(apiTS)
	if err != nil {
		t.Fatalf("reading web api.ts (%s): %v", apiTS, err)
	}
	m := webAPIVersionRE.FindSubmatch(data)
	if m == nil {
		t.Fatalf("could not find API_VERSION = 'YYYY-MM-DD' in %s", apiTS)
	}
	return apiversion.Version(m[1])
}

// TestWebAPIVersion_IsSupported asserts the version baked into the web client is
// a member of the production registry, so the two never silently drift.
func TestWebAPIVersion_IsSupported(t *testing.T) {
	webVersion := readWebAPIVersion(t)
	reg := apiversion.DefaultRegistry()
	if !reg.IsSupported(webVersion) {
		t.Errorf("web API_VERSION %q is not in the production registry %v; update services/web/src/api.ts or the registry",
			webVersion, reg.All())
	}
}

// TestWebAPIVersion_IsCurrent is the stronger guard membership alone cannot
// give (BOS-855). A web client left pinned to an older supported version still
// negotiates successfully and every gate stays green — but it receives the
// DOWN-CONVERTED response and so never executes the mirrored behavior it just
// shipped. Requiring currency turns "forgot to bump the web pin" from a silent
// behavioral divergence into a failing test.
func TestWebAPIVersion_IsCurrent(t *testing.T) {
	webVersion := readWebAPIVersion(t)
	if want := apiversion.DefaultRegistry().Current(); webVersion != want {
		t.Errorf("web API_VERSION = %q, want the registry's Current %q; bump services/web/src/api.ts whenever a new API version ships, or the web silently runs on down-converted responses",
			webVersion, want)
	}
}
