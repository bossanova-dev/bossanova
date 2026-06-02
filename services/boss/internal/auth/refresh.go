package auth

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/recurser/bossalib/authlock"
)

const (
	refreshLockTimeout    = 10 * time.Second
	refreshRequestTimeout = 10 * time.Second
)

type refreshUnlocker interface {
	Unlock() error
}

var (
	acquireRefreshLock = func(ctx context.Context) (refreshUnlocker, error) {
		return authlock.AcquireWorkOSRefreshLock(ctx)
	}
	refreshAccessToken = RefreshAccessToken
)

func (m *Manager) refreshStoredTokens(ctx context.Context, force bool) (out *Tokens, retErr error) {
	tokens, err := m.store.Load()
	if err != nil {
		return nil, err
	}
	if !force && tokens.Valid() {
		return tokens, nil
	}
	if tokens.RefreshToken == "" {
		return nil, fmt.Errorf("no refresh token available; run 'boss login'")
	}
	failedRefreshToken := tokens.RefreshToken

	lockCtx, cancel := context.WithTimeout(ctx, refreshLockTimeout)
	defer cancel()
	lock, err := acquireRefreshLock(lockCtx)
	if err != nil {
		return nil, fmt.Errorf("acquire refresh lock: %w", err)
	}
	defer func() {
		if unlockErr := lock.Unlock(); unlockErr != nil {
			retErr = errors.Join(retErr, unlockErr)
		}
	}()

	tokens, err = m.store.Load()
	if err != nil {
		retErr = err
		return nil, retErr
	}
	if !force && tokens.Valid() {
		return tokens, nil
	}
	if force && tokens.RefreshToken != "" && tokens.RefreshToken != failedRefreshToken && tokens.Valid() {
		return tokens, nil
	}
	if tokens.RefreshToken == "" {
		retErr = fmt.Errorf("no refresh token available; run 'boss login'")
		return nil, retErr
	}

	requestCtx, requestCancel := context.WithTimeout(ctx, refreshRequestTimeout)
	refreshed, err := refreshAccessToken(requestCtx, m.config, tokens.RefreshToken)
	requestCancel()
	if err != nil {
		retErr = err
		return nil, retErr
	}
	current, err := m.store.Load()
	if err != nil {
		retErr = fmt.Errorf("reload tokens before save: %w", err)
		return nil, retErr
	}
	if current.RefreshToken != tokens.RefreshToken {
		if current.Valid() {
			return current, nil
		}
		retErr = fmt.Errorf("stored refresh token changed while refreshing; run 'boss login'")
		return nil, retErr
	}
	if refreshed.RefreshToken == "" {
		refreshed.RefreshToken = tokens.RefreshToken
	}
	if refreshed.Email == "" {
		refreshed.Email = tokens.Email
	}
	if err := m.store.Save(refreshed); err != nil {
		retErr = fmt.Errorf("save refreshed tokens: %w", err)
		return nil, retErr
	}
	return refreshed, nil
}
