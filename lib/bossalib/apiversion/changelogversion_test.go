package apiversion_test

import (
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"testing"

	"github.com/recurser/bossalib/apiversion"
)

// The published changelog states the current version twice, and both have to
// move with the registry: a prose line near the top, and a "(Current)" marker on
// exactly one version heading.
var (
	changelogCurrentLineRE = regexp.MustCompile(`The current version is \*\*([0-9]{4}-[0-9]{2}-[0-9]{2})\*\*`)
	changelogCurrentMarkRE = regexp.MustCompile(`(?m)^### ([0-9]{4}-[0-9]{2}-[0-9]{2}) — .*\(Current\)\s*$`)
	changelogHeadingRE     = regexp.MustCompile(`(?m)^### ([0-9]{4}-[0-9]{2}-[0-9]{2}) — `)
)

func readChangelog(t *testing.T) (string, []byte) {
	t.Helper()
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	// thisFile = <repo>/lib/bossalib/apiversion/changelogversion_test.go
	repoRoot := filepath.Join(filepath.Dir(thisFile), "..", "..", "..")
	path := filepath.Join(repoRoot, "services", "docs", "docs", "reference", "api-changelog.mdx")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading api-changelog.mdx (%s): %v", path, err)
	}
	return path, data
}

// TestChangelogVersion_IsCurrent closes the one lockstep sync point that
// docs/api-versioning.md left as a review-checklist item.
//
// The other two are already guarded — TestOpenAPIBaseVersion_IsCurrent catches a
// missed spec bump, TestWebAPIVersion_IsCurrent a missed web pin — and the
// changelog was the remaining one a reviewer had to notice by eye. It was in
// fact missed on the V20260905 change and caught in review, which is the
// argument for gating it: the failure is invisible from inside the repository
// (every build and test stays green) and visible only to users reading published
// documentation that contradicts the version the server negotiates.
//
// Both statements are checked. The prose line is what a reader takes as the
// answer; the "(Current)" heading marker is what they navigate by. Either one
// left behind makes the page contradict itself as well as the registry.
func TestChangelogVersion_IsCurrent(t *testing.T) {
	path, data := readChangelog(t)
	want := apiversion.DefaultRegistry().Current()

	line := changelogCurrentLineRE.FindSubmatch(data)
	if line == nil {
		t.Fatalf("could not find a \"The current version is **YYYY-MM-DD**\" line in %s", path)
	}
	if got := apiversion.Version(line[1]); got != want {
		t.Errorf("changelog current-version line = %q, want the registry's Current %q; add the new version to %s whenever one ships",
			got, want, path)
	}

	marks := changelogCurrentMarkRE.FindAllSubmatch(data, -1)
	if len(marks) != 1 {
		t.Fatalf("found %d version headings marked (Current) in %s, want exactly 1", len(marks), path)
	}
	if got := apiversion.Version(marks[0][1]); got != want {
		t.Errorf("changelog (Current) heading = %q, want %q; move the marker when a new version ships", got, want)
	}
}

// TestChangelogVersions_CoverTheRegistry asserts every released version has an
// entry. A changelog whose newest entry is current but which skipped a version
// in the middle is still a documentation regression, and the currency check
// above cannot see it.
func TestChangelogVersions_CoverTheRegistry(t *testing.T) {
	path, data := readChangelog(t)

	documented := map[string]struct{}{}
	for _, m := range changelogHeadingRE.FindAllSubmatch(data, -1) {
		documented[string(m[1])] = struct{}{}
	}

	var missing []string
	for _, v := range apiversion.DefaultRegistry().All() {
		if _, ok := documented[v.String()]; !ok {
			missing = append(missing, v.String())
		}
	}
	if len(missing) > 0 {
		t.Errorf("registry versions with no changelog entry: %v; every member of the production registry needs a section in %s",
			missing, path)
	}
}
