package projectconfig

import (
	"os"
	"path/filepath"
	"testing"
)

// repositoryConfigPath is the repository's own project config, relative to
// this package. Bazel supplies it through the //:.boss-project.json data dep.
var repositoryConfigPath = filepath.Join("..", "..", "..", "..", FileName)

// TestRepositoryProjectConfigValid loads the repository's own .boss-project.json
// the way `boss project diff` and `boss project apply` do. A file the loader
// rejects stops every daemon applying this repository's crons.
func TestRepositoryProjectConfigValid(t *testing.T) {
	data, err := os.ReadFile(repositoryConfigPath)
	if err != nil {
		t.Fatal(err)
	}
	cfg, warnings, err := Decode(data)
	if err != nil {
		t.Fatalf("%s: %v", FileName, err)
	}
	if err := Validate(cfg); err != nil {
		t.Fatalf("%s is not a valid project config:\n%v", FileName, err)
	}
	for _, w := range warnings {
		t.Errorf("%s: %s", FileName, w)
	}
}
