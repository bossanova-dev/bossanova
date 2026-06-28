package buildinfo

import "testing"

func TestIsReleaseBuild(t *testing.T) {
	cases := []struct {
		version string
		want    bool
	}{
		{"v1.2.3", true},
		{"v0.0.1", true},
		{"v12.34.56", true},
		{"dev", false},
		{"unknown", false},
		{"v1.2.3-5-gabc123", false}, // ahead of tag
		{"v1.2.3-dirty", false},     // dirty tree
		{"1.2.3", false},            // missing leading v
		{"v1.2", false},             // not full semver
		{"", false},
	}
	orig := Version
	t.Cleanup(func() { Version = orig })
	for _, c := range cases {
		Version = c.version
		if got := IsReleaseBuild(); got != c.want {
			t.Errorf("IsReleaseBuild() with Version=%q = %v, want %v", c.version, got, c.want)
		}
	}
}
