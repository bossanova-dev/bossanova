package main

import (
	"context"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"testing/synctest"
	"time"

	"github.com/recurser/bossalib/kit/config"
	"github.com/recurser/bossd/internal/session"
)

func TestRun_StartupWorktreeReconcileDoesNotBlockReady_Sessions_AC222901(t *testing.T) {
	baseDir, err := os.MkdirTemp("/tmp", "bossdtest-")
	if err != nil {
		t.Fatalf("mkdir base: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(baseDir) })
	t.Setenv("HOME", baseDir)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(baseDir, ".config"))
	t.Setenv("BOSS_SETTINGS_PATH", filepath.Join(baseDir, "settings.json"))
	t.Setenv("BOSSD_ORCHESTRATOR_URL", "")

	stopSig := make(chan os.Signal, 1)
	ready := make(chan struct{})
	reconcileStarted := make(chan struct{})
	reconcileStopped := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- run(runOpts{
			stopSig:    stopSig,
			dbPath:     filepath.Join(baseDir, "bossd.db"),
			socketPath: filepath.Join(baseDir, "bossd.sock"),
			plugins:    []config.PluginConfig{},
			onReady:    func() { close(ready) },
			startupWorktreeReconcile: func(ctx context.Context) (session.UnrecordedWorktreeReport, error) {
				close(reconcileStarted)
				<-ctx.Done()
				close(reconcileStopped)
				return session.UnrecordedWorktreeReport{}, ctx.Err()
			},
		})
	}()

	select {
	case <-ready:
	case err := <-done:
		t.Fatalf("run exited before ready: %v", err)
	case <-time.After(15 * time.Second):
		t.Fatal("daemon did not reach ready state while the startup worktree reconcile was held open")
	}
	select {
	case <-reconcileStarted:
	case <-time.After(5 * time.Second):
		t.Fatal("startup worktree reconcile did not start")
	}

	stopSig <- syscall.SIGTERM
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("run returned error: %v", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("run did not return after SIGTERM")
	}
	select {
	case <-reconcileStopped:
	default:
		t.Fatal("startup worktree reconcile was not cancelled before shutdown completed")
	}
}

func TestWorktreeReconcileRunsDaily_Sessions_AC222901(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		calls := 0
		done := make(chan struct{})
		go func() {
			defer close(done)
			runDailyWorktreeReconcile(ctx, func(context.Context) (session.UnrecordedWorktreeReport, error) {
				calls++
				return session.UnrecordedWorktreeReport{}, nil
			}, time.NewTicker(24*time.Hour))
		}()
		synctest.Wait()
		if calls != 1 {
			t.Fatalf("startup calls=%d", calls)
		}
		time.Sleep(24 * time.Hour)
		synctest.Wait()
		if calls != 2 {
			t.Fatalf("daily calls=%d", calls)
		}
		cancel()
		<-done
	})
}
