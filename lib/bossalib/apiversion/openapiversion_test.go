package apiversion_test

import (
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"

	"github.com/recurser/bossalib/apiversion"
)

var openAPIVersionRE = regexp.MustCompile(`^[0-9]{4}-[0-9]{2}-[0-9]{2}$`)

func repoRoot(t *testing.T) string {
	t.Helper()
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	return filepath.Join(filepath.Dir(thisFile), "..", "..", "..")
}

// readOpenAPIInfoVersion extracts the top-level info.version from an OpenAPI
// document. It deliberately scans only the column-0 info block so nested schema
// fields named version do not satisfy this guard accidentally.
func readOpenAPIInfoVersion(t *testing.T, file string) apiversion.Version {
	t.Helper()
	data, err := os.ReadFile(file)
	if err != nil {
		t.Fatalf("reading OpenAPI spec (%s): %v", file, err)
	}

	inInfo := false
	for _, line := range strings.Split(string(data), "\n") {
		if strings.HasPrefix(line, "info:") {
			inInfo = true
			continue
		}
		if inInfo && line != "" && line[0] != ' ' {
			break
		}
		if !inInfo || !strings.HasPrefix(line, "  version:") {
			continue
		}
		raw := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(line), "version:"))
		version := strings.Trim(raw, `"'`)
		if !openAPIVersionRE.MatchString(version) {
			t.Fatalf("OpenAPI info.version in %s = %q, want YYYY-MM-DD", file, raw)
		}
		return apiversion.Version(version)
	}

	t.Fatalf("could not find top-level info.version in %s", file)
	return ""
}

// TestOpenAPIBaseVersion_IsCurrent asserts the published OpenAPI base advertises
// the registry's current version. A green here does not prove the changelog entry
// or web API_VERSION pin moved; those remain separate release checklist items.
func TestOpenAPIBaseVersion_IsCurrent(t *testing.T) {
	base := filepath.Join(repoRoot(t), "services", "docs", "openapi", "base.openapi.yaml")
	baseVersion := readOpenAPIInfoVersion(t, base)
	if want := apiversion.DefaultRegistry().Current(); baseVersion != want {
		t.Errorf("OpenAPI base info.version = %q, want registry Current %q; bump services/docs/openapi/base.openapi.yaml or the published reference advertises a version the registry does not serve",
			baseVersion, want)
	}
}

// TestOpenAPIGeneratedVersion_MatchesBase asserts `make generate` folded the
// hand-authored base version into the checked-in generated OpenAPI document.
func TestOpenAPIGeneratedVersion_MatchesBase(t *testing.T) {
	root := repoRoot(t)
	base := filepath.Join(root, "services", "docs", "openapi", "base.openapi.yaml")
	generated := filepath.Join(root, "services", "docs", "openapi", "bossanova.v1.openapi.yaml")

	baseVersion := readOpenAPIInfoVersion(t, base)
	generatedVersion := readOpenAPIInfoVersion(t, generated)
	if generatedVersion != baseVersion {
		t.Errorf("generated OpenAPI info.version = %q, want base info.version %q; run make generate after editing services/docs/openapi/base.openapi.yaml",
			generatedVersion, baseVersion)
	}
}
