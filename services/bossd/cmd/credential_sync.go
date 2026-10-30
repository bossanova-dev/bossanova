package main

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"

	"github.com/recurser/bossalib/gen/bossanova/v1/bossanovav1connect"
	server "github.com/recurser/bossd/api/daemon"
	"github.com/recurser/bossd/api/upstream"
	"github.com/recurser/bossd/internal/credsync"
	"github.com/recurser/bossd/internal/domain"
	"github.com/rs/zerolog/log"
)

type daemonCredentialSync struct {
	w                *daemonWiring
	remote           *upstream.CredentialRemote
	daemonID, userID string
	mu               sync.Mutex
	cachedEngine     *credsync.Engine
	cachedDeviceKey  string
}

func (s *daemonCredentialSync) engine(ctx context.Context) (*credsync.Engine, error) {
	key, err := upstream.LoadDeviceKey(s.daemonID)
	if err != nil {
		return nil, err
	}
	if s.cachedEngine != nil && s.cachedDeviceKey == string(key.PublicKey().Bytes()) {
		return s.cachedEngine, nil
	}
	devices, err := s.remote.ListDevices(ctx)
	if err != nil {
		return nil, err
	}
	trust := upstream.CredentialTrust{Namespace: s.userID + "\x00" + s.daemonID}
	root := ""
	for _, d := range devices {
		if (d.Status == "enrolled" || d.Status == "revoked") && d.ApprovedByDaemonID == d.DaemonID {
			if _, err := trust.PinnedIdentity(d.DaemonID); err == nil {
				root = d.DaemonID
				break
			}
		}
	}
	if root == "" {
		return nil, fmt.Errorf("credential bootstrap identity is not locally pinned; confirm its fingerprint with device approve")
	}
	identityKey, err := upstream.LoadDaemonIdentityKey(s.daemonID)
	if err != nil {
		return nil, err
	}
	s.cachedDeviceKey = string(key.PublicKey().Bytes())
	s.cachedEngine = credsync.NewEngine(credsync.Config{UserID: s.userID, DaemonID: s.daemonID, BootstrapDaemonID: root, DeviceKey: key, IdentityKey: identityKey}, s.remote, s, trust)
	return s.cachedEngine, nil
}
func (s *daemonCredentialSync) Receive(ctx context.Context, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, err := s.engine(ctx)
	if err != nil {
		return err
	}
	return e.Receive(ctx, id)
}
func (s *daemonCredentialSync) Publish(ctx context.Context, id, local string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, err := s.engine(ctx)
	if err != nil {
		return err
	}
	return e.Publish(ctx, id, local)
}
func (s *daemonCredentialSync) DevicesChanged(ctx context.Context, revoked string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, err := s.engine(ctx)
	if err != nil {
		return err
	}
	if revoked != "" {
		return e.Roll(ctx, revoked)
	}
	return e.EnsureCredentialKey(ctx)
}
func (s *daemonCredentialSync) Lookup(ctx context.Context, id string) (credsync.LocalCredential, error) {
	rows, err := s.w.accounts.List(ctx)
	if err != nil {
		return credsync.LocalCredential{}, err
	}
	for _, a := range rows {
		if a.SyncedAgentAccountID == id {
			return credsync.LocalCredential{LocalID: a.ID, SyncedCredentialVersion: a.SyncedCredentialVersion, IsAuthInvalid: a.AuthCheck.IsAuthInvalid()}, nil
		}
	}
	return credsync.LocalCredential{}, credsync.ErrNotFound
}
func (s *daemonCredentialSync) Read(_ context.Context, id string) ([]byte, error) {
	return s.w.accountCreds.Load(id)
}
func (s *daemonCredentialSync) SetVersion(ctx context.Context, id string, v int64) error {
	return s.w.accounts.SetCredentialVersion(ctx, id, v)
}
func (s *daemonCredentialSync) Install(ctx context.Context, meta credsync.AgentAccountMetadata, blob []byte, v int64) (installErr error) {
	created := false
	createdLocalID := ""
	defer func() {
		if created && installErr != nil {
			_ = s.w.accountCreds.Delete(createdLocalID)
			_ = s.w.accounts.Delete(ctx, createdLocalID)
		}
	}()
	local, err := s.Lookup(ctx, meta.SyncedAgentAccountID)
	if err != nil && !errors.Is(err, credsync.ErrNotFound) {
		return err
	}
	if err == nil && local.SyncedCredentialVersion == 0 && !local.IsAuthInvalid {
		return nil
	}
	if err == nil && local.SyncedCredentialVersion > v {
		return credsync.ErrRollback
	}
	if errors.Is(err, credsync.ErrNotFound) {
		a, createErr := s.w.accounts.Create(ctx, domain.CreateAgentAccountParams{Provider: domain.AgentAccountProvider(meta.Provider), Label: meta.Label, Priority: meta.Priority})
		if createErr != nil {
			return createErr
		}
		local.LocalID = a.ID
		created = true
		createdLocalID = a.ID
		if err := s.w.accounts.SetSync(ctx, a.ID, meta.SyncedAgentAccountID, 1); err != nil {
			return err
		}
		status := domain.AgentAccountStatus(meta.DesiredStatus)
		if _, err := s.w.accounts.Update(ctx, a.ID, domain.UpdateAgentAccountParams{Status: &status}); err != nil {
			return err
		}
	}
	return s.w.accountCreds.WithCredentialLock(local.LocalID, func() error {
		current, err := s.w.accounts.Get(ctx, local.LocalID)
		if err != nil {
			return err
		}
		if current.SyncedCredentialVersion > v {
			return credsync.ErrRollback
		}
		if !created && current.SyncedCredentialVersion == 0 && !current.AuthCheck.IsAuthInvalid() {
			return nil
		}
		if err := s.w.accountCreds.Save(local.LocalID, blob); err != nil {
			return err
		}
		if err := s.w.accounts.SetCredentialVersion(ctx, local.LocalID, v); err != nil {
			return err
		}
		if err := s.w.accounts.MarkAuthCheckCleared(ctx, local.LocalID); err != nil {
			return err
		}
		s.w.streamBus.Publish(upstream.StreamEvent{AgentAccounts: &upstream.AgentAccountsEvent{}})
		return nil
	})
}

// The namespace is the authenticated issuer and subject shared by all user daemons.
func credentialUserNamespace(jwt string) string {
	parts := strings.Split(jwt, ".")
	if len(parts) != 3 {
		return ""
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return ""
	}
	var c struct {
		Issuer  string `json:"iss"`
		Subject string `json:"sub"`
	}
	if json.Unmarshal(raw, &c) != nil || c.Subject == "" {
		return ""
	}
	return c.Issuer + "\x00" + c.Subject
}
func pinOwnCredentialIdentity(id string, key ed25519.PrivateKey) error {
	if len(key) != ed25519.PrivateKeySize {
		return fmt.Errorf("daemon identity unavailable")
	}
	return upstream.PinCredentialIdentity(id, ed25519.PublicKey(key[ed25519.SeedSize:]))
}

type daemonRefreshGate struct{ w *daemonWiring }

func (g daemonRefreshGate) synced(ctx context.Context, id string) (string, error) {
	a, err := g.w.accounts.Get(ctx, id)
	if err != nil {
		return "", err
	}
	if a.SyncedAgentAccountID != "" && g.w.credentialSync == nil {
		return "", errors.New("credential synchronization unavailable")
	}
	return a.SyncedAgentAccountID, nil
}
func (g daemonRefreshGate) Receive(ctx context.Context, id string) error {
	synced, err := g.synced(ctx, id)
	if err != nil || synced == "" {
		return err
	}
	err = g.w.credentialSync.Receive(ctx, synced)
	if errors.Is(err, credsync.ErrNotFound) {
		return nil
	}
	return err
}
func (g daemonRefreshGate) Acquire(ctx context.Context, id string, fingerprint []byte, attemptID string) (bool, error) {
	synced, err := g.synced(ctx, id)
	if err != nil {
		return false, err
	}
	if synced == "" {
		return true, nil
	}
	a, err := g.w.accounts.Get(ctx, id)
	if err != nil {
		return false, err
	}
	s := g.w.credentialSync
	expected := a.SyncedCredentialVersion
	if expected == 0 {
		s.mu.Lock()
		e, err := s.engine(ctx)
		if err == nil {
			expected, err = e.ResolveRefreshVersion(ctx, synced, id)
		}
		s.mu.Unlock()
		if err != nil {
			return false, err
		}
	}
	return s.remote.AcquireRefresh(ctx, synced, expected, fingerprint, attemptID)
}

type credentialCommandForwarder struct{ w *daemonWiring }

func (f credentialCommandForwarder) Receive(ctx context.Context, id string) error {
	if f.w.credentialSync == nil {
		return fmt.Errorf("credential sync unavailable")
	}
	return f.w.credentialSync.Receive(ctx, id)
}
func (f credentialCommandForwarder) DevicesChanged(ctx context.Context, id string) error {
	if f.w.credentialSync == nil {
		return fmt.Errorf("credential sync unavailable")
	}
	return f.w.credentialSync.DevicesChanged(ctx, id)
}

func (s *daemonCredentialSync) Reconnect(ctx context.Context) error {
	if err := s.DevicesChanged(ctx, ""); err != nil {
		return err
	}
	accounts, err := s.remote.ListAgentAccounts(ctx)
	if err != nil {
		return err
	}
	for _, a := range accounts {
		if !a.IsInScope {
			continue
		}
		local, err := s.Lookup(ctx, a.SyncedAgentAccountID)
		if err == nil && local.SyncedCredentialVersion == 0 {
			if err := s.Publish(ctx, a.SyncedAgentAccountID, local.LocalID); err != nil {
				return err
			}
		} else {
			if err := s.Receive(ctx, a.SyncedAgentAccountID); err != nil && !errors.Is(err, credsync.ErrNotFound) {
				return err
			}
		}
	}
	return nil
}

func (f credentialCommandForwarder) PublishLocal(ctx context.Context, id string) error {
	if f.w.credentialSync == nil {
		return nil
	}
	a, err := f.w.accounts.Get(ctx, id)
	if err != nil {
		return err
	}
	if a.SyncedAgentAccountID == "" || a.SyncedCredentialVersion != 0 {
		return nil
	}
	return f.w.credentialSync.Publish(ctx, a.SyncedAgentAccountID, id)
}

func (w *daemonWiring) wireCredentialDistribution(cfg *upstream.Config, client bossanovav1connect.OrchestratorServiceClient, sessionTokenHolder *upstream.SessionTokenHolder, identityKey ed25519.PrivateKey, authToken string) {
	w.credentialDevices = server.NewCredentialDeviceService(server.CredentialDeviceConfig{
		DaemonID: cfg.DaemonID, IdentityKey: identityKey, Client: client,
		LoadKey: upstream.LoadDeviceKey, RotateKey: upstream.RotateDeviceKey,
		SessionToken: sessionTokenHolder.Get,
		PinIdentity:  upstream.PinCredentialIdentity,
		OnTrustChanged: func(ctx context.Context) {
			if w.credentialSync != nil {
				if err := w.credentialSync.Reconnect(ctx); err != nil {
					log.Warn().Err(err).Msg("credential sync after trust confirmation deferred")
				}
			}
		},
	})
	if namespace := credentialUserNamespace(authToken); namespace != "" {
		if err := pinOwnCredentialIdentity(cfg.DaemonID, identityKey); err != nil {
			log.Warn().Err(err).Msg("credential identity pin unavailable")
		}
		w.credentialSync = &daemonCredentialSync{w: w, remote: &upstream.CredentialRemote{Client: client, Token: sessionTokenHolder.Get, DaemonID: cfg.DaemonID}, daemonID: cfg.DaemonID, userID: namespace}
	}
	w.credentialEnrollment = newCredentialEnrollmentWorker(w.credentialDevices.EnsureEnrollment, func(ctx context.Context) error {
		if w.credentialSync == nil {
			return nil
		}
		return w.credentialSync.Reconnect(ctx)
	})
}

// PublishRefresh serializes with Receive and holds the credential lock in that
// same order. A concurrent operator reauthentication wins over a stale result.
func (g daemonRefreshGate) PublishRefresh(ctx context.Context, id string, blob []byte, consumed []byte) error {
	synced, err := g.synced(ctx, id)
	if err != nil || synced == "" {
		return err
	}
	s := g.w.credentialSync
	s.mu.Lock()
	defer s.mu.Unlock()
	e, err := s.engine(ctx)
	if err != nil {
		return err
	}
	err = g.w.accountCreds.WithCredentialLock(id, func() error {
		current, err := g.w.accountCreds.Load(id)
		if err != nil {
			return err
		}
		if !bytes.Equal(current, blob) {
			return credsync.ErrConflict
		}
		a, err := g.w.accounts.Get(ctx, id)
		if err != nil {
			return err
		}
		if a.SyncedCredentialVersion == 0 {
			return s.remote.ReleaseRefresh(ctx, synced)
		}
		if err = e.PublishRefresh(ctx, synced, id, blob, consumed); err != nil {
			return err
		}
		return s.remote.ReleaseRefresh(ctx, synced)
	})
	if errors.Is(err, credsync.ErrCredentialReplaced) {
		if receiveErr := e.Receive(ctx, synced); receiveErr != nil {
			return receiveErr
		}
	}
	return err
}
