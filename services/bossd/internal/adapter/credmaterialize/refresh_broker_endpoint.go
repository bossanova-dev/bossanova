package credmaterialize

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"net"
	"strconv"
	"time"

	"github.com/rs/zerolog"
)

// refreshBrokerEndpoint is the broker's durable network identity. A managed
// Codex process holds the URL in its environment and the broker id inside its
// capability, so both must outlive a daemon restart for that process to keep
// refreshing. Neither value is a secret: the per-account journal seed is what
// signs capabilities.
type refreshBrokerEndpoint struct {
	BrokerID string `json:"broker_id"`
	Port     int    `json:"port"`
}

// refreshBrokerRebind bounds how long a restarting daemon waits for its
// predecessor to release the persisted port before rotating the endpoint.
type refreshBrokerRebind struct {
	window time.Duration
	step   time.Duration
}

var defaultRefreshBrokerRebind = refreshBrokerRebind{window: 2 * time.Second, step: 100 * time.Millisecond}

// decodeRefreshBrokerEndpoint accepts only a complete, in-range endpoint.
func decodeRefreshBrokerEndpoint(raw []byte) (refreshBrokerEndpoint, bool) {
	var e refreshBrokerEndpoint
	if len(raw) == 0 || json.Unmarshal(raw, &e) != nil || e.BrokerID == "" || e.Port < 1 || e.Port > 65535 {
		return refreshBrokerEndpoint{}, false
	}
	return e, true
}

// openRefreshBrokerEndpoint reclaims the persisted endpoint, or rotates to a
// fresh broker id on an ephemeral loopback port. Rotation invalidates every
// capability an earlier daemon issued, which is the fail-closed outcome when
// the old port may now belong to another listener. That invalidation is only
// durable once the rotated endpoint is saved. When a stored endpoint could come
// back on a later start (a readable endpoint whose port was taken, or a load
// error that may clear) and the save fails, the broker does not start: it never
// serves, announces or mints capabilities under a rotation a later restart
// would undo. The old endpoint stays persisted, so a later restart that finds
// its port free reclaims it, which is the same restart-gap exposure as any
// daemon outage. Missing or undecodable state can never be reclaimed, so that
// rotation serves even unsaved.
func openRefreshBrokerEndpoint(ctx context.Context, store RefreshBrokerStore, logger zerolog.Logger, rebind refreshBrokerRebind) (net.Listener, string, error) {
	raw, err := store.LoadRefreshBrokerEndpoint(ctx)
	reason, revivable := "endpoint state missing", false
	switch persisted, ok := decodeRefreshBrokerEndpoint(raw); {
	case err != nil:
		reason, revivable = "endpoint state unreadable", true
	case len(raw) > 0 && !ok:
		reason = "endpoint state unreadable"
	case ok:
		if listener := reclaimRefreshBrokerPort(ctx, persisted.Port, rebind); listener != nil {
			return listener, persisted.BrokerID, nil
		}
		reason, revivable = "endpoint port unavailable", true
	}
	return rotateRefreshBrokerEndpoint(ctx, store, logger, reason, revivable)
}

// reclaimRefreshBrokerPort retries the bind for a bounded window so a restart
// overlap can let the previous daemon release the port. It returns nil when
// the port is still taken after the window.
func reclaimRefreshBrokerPort(ctx context.Context, port int, rebind refreshBrokerRebind) net.Listener {
	address := net.JoinHostPort("127.0.0.1", strconv.Itoa(port))
	deadline := time.Now().Add(rebind.window)
	for {
		if listener, err := net.Listen("tcp4", address); err == nil {
			return listener
		}
		if !time.Now().Before(deadline) {
			return nil
		}
		timer := time.NewTimer(rebind.step)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil
		case <-timer.C:
		}
	}
}

// rotateRefreshBrokerEndpoint persists the rotated endpoint before returning
// its listener, so no capability is minted under an unpersisted rotation away
// from a stored endpoint a later start could reclaim (revivable).
func rotateRefreshBrokerEndpoint(ctx context.Context, store RefreshBrokerStore, logger zerolog.Logger, reason string, revivable bool) (net.Listener, string, error) {
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		return nil, "", errors.New("credential refresh listener unavailable")
	}
	addr, ok := listener.Addr().(*net.TCPAddr)
	if !ok {
		_ = listener.Close()
		return nil, "", errors.New("credential refresh listener unavailable")
	}
	next := refreshBrokerEndpoint{BrokerID: rand.Text(), Port: addr.Port}
	raw, err := json.Marshal(next)
	if err == nil {
		err = store.SaveRefreshBrokerEndpoint(ctx, raw)
	}
	if err != nil && revivable {
		_ = listener.Close()
		logger.Warn().Err(err).Str("reason", reason).Msg("credential refresh broker endpoint rotation not persisted; broker not started")
		return nil, "", errors.New("credential refresh broker endpoint rotation not persisted")
	}
	logger.Warn().Str("reason", reason).Int("port", next.Port).Msg("credential refresh broker endpoint rotated; Codex sessions started before this daemon start must be relaunched")
	if err != nil {
		logger.Warn().Msg("credential refresh broker endpoint not persisted; Codex sessions will need relaunching after the next daemon restart")
	}
	return listener, next.BrokerID, nil
}
