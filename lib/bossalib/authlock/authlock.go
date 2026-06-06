package authlock

import (
	"context"
	"fmt"
	"time"

	"github.com/gofrs/flock"
	"github.com/recurser/bossalib/appstate"
	"github.com/recurser/bossalib/config"
)

const workOSRefreshLockFile = "workos-refresh.lock"

var pollInterval = 50 * time.Millisecond
var afterFailedTryLock func()

// Lock is an acquired cross-process lock.
type Lock struct {
	file *flock.Flock
}

// AcquireWorkOSRefreshLock acquires the shared WorkOS refresh lock.
func AcquireWorkOSRefreshLock(ctx context.Context) (*Lock, error) {
	settings, err := config.Load()
	if err != nil {
		return nil, fmt.Errorf("load settings for refresh lock: %w", err)
	}
	return AcquireWorkOSRefreshLockForSettings(ctx, settings)
}

// AcquireWorkOSRefreshLockForSettings acquires the shared WorkOS refresh lock
// from the global appstate path. WorkOS tokens are shared through the host
// keyring, so the refresh lock must stay shared across profiles.
func AcquireWorkOSRefreshLockForSettings(ctx context.Context, settings config.Settings) (*Lock, error) {
	path, err := workOSRefreshLockPath(settings)
	if err != nil {
		return nil, err
	}
	return acquire(ctx, path, pollInterval)
}

func workOSRefreshLockPath(settings config.Settings) (string, error) {
	if _, _, err := config.ConfiguredAppDataDir(settings); err != nil {
		return "", err
	}
	return appstate.Path(workOSRefreshLockFile)
}

func acquire(ctx context.Context, path string, interval time.Duration) (*Lock, error) {
	if interval <= 0 {
		interval = pollInterval
	}
	file := flock.New(path)
	for {
		if err := ctx.Err(); err != nil {
			return nil, fmt.Errorf("acquire refresh lock %q: %w", path, err)
		}
		locked, err := file.TryLock()
		if err != nil {
			return nil, fmt.Errorf("acquire refresh lock %q: %w", path, err)
		}
		if locked {
			return &Lock{file: file}, nil
		}
		if afterFailedTryLock != nil {
			afterFailedTryLock()
		}
		timer := time.NewTimer(interval)
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			return nil, fmt.Errorf("acquire refresh lock %q: %w", path, ctx.Err())
		case <-timer.C:
		}
	}
}

// Unlock releases the lock.
func (l *Lock) Unlock() error {
	if l == nil || l.file == nil {
		return nil
	}
	if err := l.file.Unlock(); err != nil {
		return fmt.Errorf("release refresh lock: %w", err)
	}
	return nil
}
