package e2e_test

import (
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/recurser/boss/internal/tuitest"
)

// TestBaseHarnessEnvKeepsBossStateInTheThrowawayHome runs the built boss with
// the harness env while the developer's XDG base directories point elsewhere:
// boss makes its log directory under the throwaway HOME and writes nothing into
// those directories.
func TestBaseHarnessEnvKeepsBossStateInTheThrowawayHome(t *testing.T) {
	bin, err := tuitest.BossBinaryPath()
	if err != nil {
		t.Fatal(err)
	}
	developer := t.TempDir()
	xdgDirs := []string{"XDG_CONFIG_HOME", "XDG_STATE_HOME", "XDG_CACHE_HOME", "XDG_DATA_HOME"}
	for _, name := range xdgDirs {
		t.Setenv(name, filepath.Join(developer, name))
	}
	home := t.TempDir()

	env := tuitest.BaseHarnessEnv(os.Environ())
	for _, e := range env {
		for _, name := range xdgDirs {
			if strings.HasPrefix(e, name+"=") {
				t.Errorf("the harness env passes %s through", e)
			}
		}
	}
	env = append(env, "HOME="+home, "BOSS_SKIP_SKILLS=1")
	cmd := exec.Command(bin, "--help")
	cmd.Env = env
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("boss --help: %v\n%s", err, out)
	}

	if info, err := os.Stat(filepath.Join(home, ".local", "state", "bossanova", "logs")); err != nil || !info.IsDir() {
		t.Fatalf("boss made no log directory under the throwaway HOME: %v", err)
	}
	err = filepath.WalkDir(developer, func(path string, _ fs.DirEntry, err error) error {
		if err == nil && path != developer {
			t.Errorf("boss wrote %s into the developer's XDG directories", path)
		}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
}
