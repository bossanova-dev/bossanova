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

// TestWebAPIVersion_IsSupported asserts the version baked into the web client is
// a member of the production registry, so the two never silently drift.
func TestWebAPIVersion_IsSupported(t *testing.T) {
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
	webVersion := apiversion.Version(m[1])

	reg := apiversion.DefaultRegistry()
	if !reg.IsSupported(webVersion) {
		t.Errorf("web API_VERSION %q is not in the production registry %v; update services/web/src/api.ts or the registry",
			webVersion, reg.All())
	}
}
