package realstack

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sync"
	"time"
)

// Binaries are built from this checkout, never from the developer's installation.
type Binaries struct{ Bossd, Stub, GH, SeedDB, TemplateDB string }

var builds struct {
	sync.Mutex
	binaries Binaries
	err      error
}

// Build compiles the fixture programs and migrates one reusable template database.
// Call once in TestMain; the returned cleanup runs after all stacks have stopped.
func Build() (func(), error) {
	builds.Lock()
	defer builds.Unlock()
	if builds.binaries.Bossd != "" {
		return func() {}, builds.err
	}
	dir, err := os.MkdirTemp("", "boss-realstack-build-")
	if err != nil {
		return func() {}, err
	}
	cleanup := func() {
		if err := removeDir(dir); err != nil {
			fmt.Fprintf(os.Stderr, "realstack build cleanup: %v\n", err)
		}
	}
	_, source, _, _ := runtime.Caller(0)
	root := filepath.Clean(filepath.Join(filepath.Dir(source), "../../../.."))
	b := Binaries{Bossd: filepath.Join(dir, "bossd"), Stub: filepath.Join(dir, "bossd-plugin-stub-runner"), GH: filepath.Join(dir, "gh"), SeedDB: filepath.Join(dir, "bossd-seeddb"), TemplateDB: filepath.Join(dir, "template.db")}
	for _, build := range []struct{ module, target, output string }{{"services/bossd", "./cmd", b.Bossd}, {"plugins/bossd-plugin-stub-runner", ".", b.Stub}, {"services/boss", "./internal/realstack/fakegh", b.GH}, {"services/bossd", "./internal/dbtest/seeddb", b.SeedDB}} {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
		cmd := exec.CommandContext(ctx, "go", "build", "-o", build.output, build.target)
		cmd.Dir = filepath.Join(root, build.module)
		cmd.WaitDelay = time.Second
		output, runErr := cmd.CombinedOutput()
		cancel()
		if runErr != nil {
			builds.err = fmt.Errorf("build %s: %w\n%s", build.target, runErr, output)
			return cleanup, builds.err
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, b.SeedDB, "--db", b.TemplateDB, "--migrate-only")
	cmd.WaitDelay = time.Second
	if output, err := cmd.CombinedOutput(); err != nil {
		return cleanup, fmt.Errorf("migrate template: %w\n%s", err, output)
	}
	builds.binaries = b
	return cleanup, nil
}
