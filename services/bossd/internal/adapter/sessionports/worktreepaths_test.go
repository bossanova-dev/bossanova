package sessionports

import "testing"

func TestParseCWDPaths(t *testing.T) {
	paths, err := parseCWDPaths("p123\nfcwd\nn/a path\np456\nn/other\n")
	if err != nil || len(paths) != 2 || paths[0] != "/a path" || paths[1] != "/other" {
		t.Fatalf("paths=%v err=%v", paths, err)
	}
	for _, bad := range []string{"p123\nnrelative\n", "p123\nxunexpected\n", "p123\nfcwd\nn/proc/123/cwd (readlink: Permission denied)\n", "p123\nfcwd\nn/proc/123/cwd\n"} {
		if _, err := parseCWDPaths(bad); err == nil {
			t.Fatalf("accepted %q", bad)
		}
	}
}
