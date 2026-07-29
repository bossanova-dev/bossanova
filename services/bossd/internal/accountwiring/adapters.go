// Package accountwiring adapts the bossd daemon's concrete stores and plugin
// clients to the degrade-safe account.Resolver seams (Registry, Materializer)
// and exposes the session-aware spawn-env resolver consumed by the session
// Lifecycle and the plugin HostService.
//
// It lives outside internal/account so the resolver stays a pure library with
// no dependency on db, agent, or the generated protobufs; this package holds
// all the daemon-specific plumbing. It is imported only by cmd/main.go.
//
// Nothing here ever logs a credential blob or a materialized env value.
package accountwiring

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/rs/zerolog"
	"google.golang.org/grpc/codes"
	grpcstatus "google.golang.org/grpc/status"

	bossanovav1 "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/models"

	"github.com/recurser/bossd/internal/account"
	"github.com/recurser/bossd/internal/agent"
	"github.com/recurser/bossd/internal/credmaterialize"
	"github.com/recurser/bossd/internal/db"
	"github.com/recurser/bossd/internal/session"
)

// AccountStore is the narrow slice of db.AccountStore the adapters read. The
// daemon's *db.SQLiteAccountStore satisfies it.
type AccountStore interface {
	List(ctx context.Context) ([]*models.Account, error)
	Get(ctx context.Context, id string) (*models.Account, error)
	Update(ctx context.Context, id string, params db.UpdateAccountParams) (*models.Account, error)
}

type usageProbeStore interface {
	Get(ctx context.Context, id string) (*models.Account, error)
	RecordUsageProbe(ctx context.Context, id string, snap models.UsageSnapshot) error
	MarkAccountSuspended(ctx context.Context, id string, reason string) error
}

// accountSuspender fails an account's health with a legible reason. The usage
// probe uses it to proactively sideline an account the plugin confirms is
// suspended (an org/billing block), so rotation and manual `--refresh` alike
// stop selecting it.
type accountSuspender interface {
	MarkAccountSuspended(ctx context.Context, id string, reason string) error
}

// IsSuspension reports whether err is the claude plugin's confirmed
// account-suspension signal: a gRPC codes.PermissionDenied. The plugin reserves
// that code exclusively for a 403 whose body carries the suspension signature
// (see plugins/bossd-plugin-claude/probe.go claudeAccountSuspendedError), so it
// is the single source of truth for the plugin→daemon suspension contract.
// grpcstatus.Code unwraps the %w chain.
func IsSuspension(err error) bool {
	return err != nil && grpcstatus.Code(err) == codes.PermissionDenied
}

// MarkSuspendedIfConfirmed is the single reaction point for the suspension
// contract, shared by the periodic rotation refresh (cmd) and the manual
// --refresh adapter path: when probeErr is a confirmed suspension it fails the
// account's health with the plugin's legible reason. handled reports whether
// probeErr was a suspension (regardless of the mark result); reason is the
// plugin message; err is any error from the health write.
func MarkSuspendedIfConfirmed(ctx context.Context, store accountSuspender, id string, probeErr error) (handled bool, reason string, err error) {
	if !IsSuspension(probeErr) {
		return false, "", nil
	}
	reason = grpcstatus.Convert(probeErr).Message()
	if merr := store.MarkAccountSuspended(ctx, id, reason); merr != nil {
		return true, reason, fmt.Errorf("mark account suspended: %w", merr)
	}
	return true, reason, nil
}

// CredentialStore loads and saves opaque account credential blobs. Satisfied
// by the keyring-backed accountcred.Store. Blobs are never logged.
type CredentialStore interface {
	Load(accountID string) ([]byte, error)
	Save(accountID string, blob []byte) error
}

// --- Registry adapter -----------------------------------------------------

type registryAdapter struct{ store AccountStore }

// NewRegistry adapts an AccountStore to account.Registry. A nil store yields a
// registry that reports no accounts (the resolver then degrades to account 0).
func NewRegistry(store AccountStore) account.Registry { return &registryAdapter{store: store} }

func (a *registryAdapter) List(ctx context.Context) ([]account.AccountMeta, error) {
	if a.store == nil {
		return nil, nil
	}
	rows, err := a.store.List(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]account.AccountMeta, 0, len(rows))
	for _, r := range rows {
		out = append(out, toMeta(r))
	}
	return out, nil
}

func (a *registryAdapter) Get(ctx context.Context, id string) (account.AccountMeta, bool, error) {
	if a.store == nil {
		return account.AccountMeta{}, false, nil
	}
	r, err := a.store.Get(ctx, id)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return account.AccountMeta{}, false, nil
		}
		return account.AccountMeta{}, false, err
	}
	return toMeta(r), true, nil
}

func (a *registryAdapter) TouchLastUsed(ctx context.Context, id string, at time.Time) error {
	if a.store == nil {
		return nil
	}
	// db.UpdateAccountParams.LastUsedAt is **time.Time: a non-nil outer pointer
	// selects the column, and the inner pointer's value is written.
	tp := &at
	_, err := a.store.Update(ctx, id, db.UpdateAccountParams{LastUsedAt: &tp})
	return err
}

func toMeta(a *models.Account) account.AccountMeta {
	m := account.AccountMeta{
		ID:           a.ID,
		Provider:     string(a.Provider),
		Label:        a.Label,
		Status:       string(a.Status),
		Health:       string(a.Health),
		Priority:     a.Priority,
		CoolingUntil: a.CooldownUntil,
		LastUsedAt:   a.LastUsedAt,
	}
	if u := a.Usage; u != nil {
		m.Util5h = u.Util5h
		m.Util7d = u.Util7d
		m.UsageFetchedAt = u.FetchedAt
		m.Reset5h = u.Reset5h
		m.Reset7d = u.Reset7d
	}
	return m
}

// --- Materializer adapter -------------------------------------------------

type materializerAdapter struct {
	clients map[string]agent.AgentRunnerClient
	store   AccountStore
	creds   CredentialStore
	codex   *credmaterialize.Materializer
	log     zerolog.Logger
}

// NewMaterializer adapts the per-provider agent-runner clients, the account
// store, and the credential loader to account.Materializer. Any of them may be
// nil; the adapter then degrades to no-rotation (nil env), never erroring on a
// spawn.
func NewMaterializer(
	clients map[string]agent.AgentRunnerClient,
	store AccountStore,
	creds CredentialStore,
	log zerolog.Logger,
) account.Materializer {
	var codex *credmaterialize.Materializer
	if creds != nil {
		var err error
		codex, err = credmaterialize.New(contextCredentialStore{store: creds}, log)
		if err != nil {
			log.Warn().Err(err).Msg("account: Codex credential materializer unavailable")
		}
	}
	return &materializerAdapter{clients: clients, store: store, creds: creds, codex: codex, log: log}
}

type contextCredentialStore struct {
	store CredentialStore
}

func (s contextCredentialStore) LoadCredential(ctx context.Context, accountID string) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return s.store.Load(accountID)
}

func (s contextCredentialStore) SaveCredential(ctx context.Context, accountID string, blob []byte) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return s.store.Save(accountID, blob)
}

func (m *materializerAdapter) client(provider string) (agent.AgentRunnerClient, bool) {
	c, ok := m.clients[provider]
	return c, ok && c != nil
}

// SupportsRotation reports whether provider's plugin can materialize
// credentials. A missing client, codes.Unimplemented, or any other RPC error
// all degrade to (false, nil): an unreachable plugin must never fail a spawn.
func (m *materializerAdapter) SupportsRotation(ctx context.Context, provider string) (bool, error) {
	client, ok := m.client(provider)
	if !ok {
		return false, nil
	}
	resp, err := client.RotationCapability(ctx, &bossanovav1.RotationCapabilityRequest{})
	if err != nil {
		if grpcstatus.Code(err) != codes.Unimplemented {
			m.log.Warn().Err(err).Str("provider", provider).
				Msg("account: RotationCapability failed; treating provider as no-rotation")
		}
		return false, nil
	}
	return resp.GetSupportsRotation(), nil
}

// MaterializeAccount loads the account's credential blob from the keyring and
// asks the provider plugin to turn it into spawn env. Any failure returns
// (nil, err) so the resolver logs and degrades. The blob and the returned env
// are never logged.
func (m *materializerAdapter) MaterializeAccount(ctx context.Context, accountID string) (map[string]string, error) {
	if m.store == nil || m.creds == nil {
		return nil, nil
	}
	acct, err := m.store.Get(ctx, accountID)
	if err != nil {
		return nil, fmt.Errorf("lookup account for materialize: %w", err)
	}
	if string(acct.Provider) == "codex" {
		if m.codex == nil {
			return nil, nil
		}
		materialized, _, err := m.codex.MaterializeCodex(ctx, accountID)
		if err != nil {
			return nil, fmt.Errorf("materialize codex account: %w", err)
		}
		return materialized.Env, nil
	}
	client, ok := m.client(string(acct.Provider))
	if !ok {
		return nil, nil
	}
	blob, err := m.creds.Load(accountID)
	if err != nil {
		return nil, fmt.Errorf("load account credential: %w", err)
	}
	resp, err := client.MaterializeAccount(ctx, &bossanovav1.MaterializeAccountRequest{CredentialBlob: blob})
	if err != nil {
		return nil, fmt.Errorf("plugin MaterializeAccount: %w", err)
	}
	return resp.GetEnv(), nil
}

// RecordUsageProbe probes the currently bound account through its provider
// plugin and stores only the returned usage metadata. Credential material is
// loaded solely to produce the probe env and is never persisted or logged.
func (m *materializerAdapter) RecordUsageProbe(ctx context.Context, accountID string) error {
	store, ok := m.store.(usageProbeStore)
	if !ok {
		return nil
	}
	snap, err := m.ProbeUsageSnapshot(ctx, accountID)
	if err != nil {
		// A confirmed suspension (org/billing block) is permanent, unlike a
		// transient probe failure: fail the account's health so rotation and a
		// manual `--refresh` alike stop selecting it. Any other error is returned
		// for the caller to log and ignore (fail-soft, unchanged).
		if handled, _, merr := MarkSuspendedIfConfirmed(ctx, store, accountID, err); handled {
			return merr // nil on success; the account is now health=failed
		}
		return err
	}
	if snap.FetchedAt == nil {
		return nil
	}
	if err := store.RecordUsageProbe(ctx, accountID, snap); err != nil {
		return fmt.Errorf("record usage probe: %w", err)
	}
	return nil
}

// ProbeUsageSnapshot probes the currently bound account through its provider
// plugin and returns only normalized usage metadata. It does not persist the
// snapshot, letting callers cache the exact probe result they used for a
// decision.
func (m *materializerAdapter) ProbeUsageSnapshot(ctx context.Context, accountID string) (models.UsageSnapshot, error) {
	store, ok := m.store.(interface {
		Get(ctx context.Context, id string) (*models.Account, error)
	})
	if !ok || m.creds == nil {
		return models.UsageSnapshot{}, nil
	}
	acct, err := store.Get(ctx, accountID)
	if err != nil {
		return models.UsageSnapshot{}, fmt.Errorf("lookup account for usage probe: %w", err)
	}
	client, ok := m.client(string(acct.Provider))
	if !ok {
		return models.UsageSnapshot{}, nil
	}
	blob, err := m.creds.Load(accountID)
	if err != nil {
		return models.UsageSnapshot{}, fmt.Errorf("load account credential for usage probe: %w", err)
	}
	mat, err := client.MaterializeAccount(ctx, &bossanovav1.MaterializeAccountRequest{CredentialBlob: blob})
	if err != nil {
		return models.UsageSnapshot{}, fmt.Errorf("plugin MaterializeAccount for usage probe: %w", err)
	}
	env, cleanup, err := usageProbeCredentialEnv(mat)
	if err != nil {
		return models.UsageSnapshot{}, err
	}
	defer cleanup()
	resp, err := client.ProbeRateLimit(ctx, &bossanovav1.ProbeRateLimitRequest{CredentialEnv: env})
	if err != nil {
		// Preserve a gRPC-status error verbatim (a typed 401 auth invalidation or
		// a 403 account suspension) so callers can both classify it by code and
		// surface the plugin's reason message unpolluted by a wrapper prefix.
		// Only non-status errors get the context wrap.
		if _, ok := grpcstatus.FromError(err); ok {
			return models.UsageSnapshot{}, err
		}
		return models.UsageSnapshot{}, fmt.Errorf("plugin ProbeRateLimit: %w", err)
	}
	now := time.Now().UTC()
	return usageSnapshotFromRateLimitStatus(resp.GetStatus(), now), nil
}

func usageProbeCredentialEnv(mat *bossanovav1.MaterializeAccountResponse) (map[string]string, func(), error) {
	env := map[string]string{}
	for k, v := range mat.GetEnv() {
		env[k] = v
	}
	cleanup := func() {}
	homeKey := mat.GetHomeDirEnvKey()
	if homeKey == "" {
		return env, cleanup, nil
	}
	dir, err := os.MkdirTemp("", "boss-usage-probe-*")
	if err != nil {
		return nil, cleanup, fmt.Errorf("create usage probe home: %w", err)
	}
	cleanup = func() { _ = os.RemoveAll(dir) }
	// os.MkdirTemp already creates the directory with 0o700, so an explicit
	// re-chmod is a no-op. Dropping it clears gosec G302 without weakening the
	// permission guarantee (BOS-423).
	if err := seedUsageProbeHome(homeKey, dir); err != nil {
		cleanup()
		return nil, func() {}, err
	}
	for _, file := range mat.GetFiles() {
		rel := file.GetRelativePath()
		if rel == "" || !filepath.IsLocal(rel) {
			cleanup()
			return nil, func() {}, fmt.Errorf("materialized usage probe file has invalid relative path")
		}
		path := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			cleanup()
			return nil, func() {}, fmt.Errorf("create usage probe credential dir: %w", err)
		}
		mode := os.FileMode(file.GetMode())
		if mode == 0 {
			mode = 0o600
		}
		if err := os.WriteFile(path, file.GetContent(), mode); err != nil {
			cleanup()
			return nil, func() {}, fmt.Errorf("write usage probe credential file: %w", err)
		}
	}
	env[homeKey] = dir
	return env, cleanup, nil
}

func seedUsageProbeHome(homeKey, dstHome string) error {
	if homeKey != "CODEX_HOME" {
		return nil
	}
	srcHome := strings.TrimSpace(os.Getenv(homeKey))
	if srcHome == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return nil
		}
		srcHome = filepath.Join(home, ".codex")
	}
	for _, rel := range []string{"session_index.jsonl", "sessions"} {
		src := filepath.Join(srcHome, rel)
		dst := filepath.Join(dstHome, rel)
		if err := copyProbeSeedPath(src, dst); err != nil {
			return err
		}
	}
	return nil
}

func copyProbeSeedPath(src, dst string) error {
	resolved, err := filepath.EvalSymlinks(src)
	if err == nil {
		src = resolved
	}
	info, err := os.Stat(src)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("stat usage probe seed path: %w", err)
	}
	if info.IsDir() {
		return copyProbeSeedDir(src, dst)
	}
	return copyProbeSeedFile(src, dst, info.Mode())
}

func copyProbeSeedDir(src, dst string) error {
	return filepath.WalkDir(src, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return fmt.Errorf("walk usage probe seed dir: %w", err)
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return fmt.Errorf("rel usage probe seed path: %w", err)
		}
		target := filepath.Join(dst, rel)
		if d.IsDir() {
			if err := os.MkdirAll(target, 0o700); err != nil {
				return fmt.Errorf("create usage probe seed dir: %w", err)
			}
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return fmt.Errorf("stat usage probe seed file: %w", err)
		}
		return copyProbeSeedFile(path, target, info.Mode())
	})
}

func copyProbeSeedFile(src, dst string, mode os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
		return fmt.Errorf("create usage probe seed file dir: %w", err)
	}
	// #nosec G304 -- reads a daemon-controlled usage-probe seed; src is under its own WalkDir root, not attacker-named (secret-adjacent)
	// owner=@recurser review-by=2027-01-18 issue=BOS-28
	in, err := os.Open(src)
	if err != nil {
		return fmt.Errorf("open usage probe seed file: %w", err)
	}
	defer func() { _ = in.Close() }()
	// #nosec G304 -- writes probe-seed copy to filepath.Join(dst, rel) where rel is from filepath.Rel of a walked file; internal, not attacker-named (secret-adjacent)
	// owner=@recurser review-by=2027-01-18 issue=BOS-28
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, mode.Perm())
	if err != nil {
		return fmt.Errorf("create usage probe seed file: %w", err)
	}
	_, copyErr := io.Copy(out, in)
	closeErr := out.Close()
	if copyErr != nil {
		return fmt.Errorf("copy usage probe seed file: %w", copyErr)
	}
	if closeErr != nil {
		return fmt.Errorf("close usage probe seed file: %w", closeErr)
	}
	return nil
}

func usageSnapshotFromRateLimitStatus(status *bossanovav1.RateLimitStatus, now time.Time) models.UsageSnapshot {
	snap := models.UsageSnapshot{
		Util5h:    status.GetUtil_5H(),
		Util7d:    status.GetUtil_7D(),
		Status:    status.GetStatus().String(),
		PlanTier:  status.GetPlanTier(),
		FetchedAt: &now,
	}
	if reset := status.GetReset_5H(); reset != nil {
		t := reset.AsTime().UTC()
		snap.Reset5h = &t
	}
	if reset := status.GetReset_7D(); reset != nil {
		t := reset.AsTime().UTC()
		snap.Reset7d = &t
	}
	return snap
}

// --- Lifecycle rotation adapters -----------------------------------------

// LifecycleMaterializer adapts account.Materializer to the session Lifecycle's
// rotate-and-restart materializer seam.
type LifecycleMaterializer struct {
	materializer account.Materializer
}

// NewLifecycleMaterializer wraps materializer. A nil materializer yields nil
// env, preserving the lifecycle's fail-safe behavior.
func NewLifecycleMaterializer(materializer account.Materializer) *LifecycleMaterializer {
	return &LifecycleMaterializer{materializer: materializer}
}

// Materialize returns the spawn env for account. It never logs env values.
func (m *LifecycleMaterializer) Materialize(ctx context.Context, account *models.Account) (map[string]string, error) {
	if m == nil || m.materializer == nil || account == nil {
		return nil, nil
	}
	return m.materializer.MaterializeAccount(ctx, account.ID)
}

// RotationBindingResolver adapts the persisted session AccountID binding into
// the rotation signal shape consumed by session.Lifecycle.
type RotationBindingResolver struct {
	registry     account.Registry
	materializer account.Materializer
}

// NewRotationBindingResolver wraps the account registry/materializer for
// lifecycle rotation decisions. Missing deps degrade to unbound/no-capability.
func NewRotationBindingResolver(registry account.Registry, materializer account.Materializer) *RotationBindingResolver {
	return &RotationBindingResolver{registry: registry, materializer: materializer}
}

// CurrentBinding reports the account currently bound to session. It returns
// bound=false for account 0, missing sessions, missing registry, or a stale
// AccountID that no longer exists.
func (r *RotationBindingResolver) CurrentBinding(ctx context.Context, sess *models.Session) (session.RotationBinding, bool, error) {
	if r == nil || sess == nil || sess.AccountID == nil || *sess.AccountID == "" || r.registry == nil {
		return session.RotationBinding{}, false, nil
	}
	accountID := *sess.AccountID
	acct, ok, err := r.registry.Get(ctx, accountID)
	if err != nil {
		return session.RotationBinding{}, false, err
	}
	if !ok {
		return session.RotationBinding{}, false, nil
	}

	provider := acct.Provider
	if provider == "" {
		provider = sess.AgentName
	}
	capable := false
	if r.materializer != nil {
		supported, err := r.materializer.SupportsRotation(ctx, provider)
		if err != nil {
			return session.RotationBinding{}, false, err
		}
		capable = supported
	}
	return session.RotationBinding{
		CappedAccountID: accountID,
		Provider:        provider,
		RotationCapable: capable,
	}, true, nil
}

// --- Session-aware spawn-env resolver -------------------------------------

// SpawnEnvResolver adapts *account.Resolver to the session-aware spawn-env seam
// (Resolve(ctx, sess) map[string]string) consumed by the session Lifecycle and
// the plugin HostService. It never logs env values.
type SpawnEnvResolver struct {
	resolver *account.Resolver
	log      zerolog.Logger
}

// NewSpawnEnvResolver wraps resolver. A nil resolver yields an adapter whose
// Resolve always returns nil (system-default binding).
func NewSpawnEnvResolver(resolver *account.Resolver, log zerolog.Logger) *SpawnEnvResolver {
	return &SpawnEnvResolver{resolver: resolver, log: log}
}

// Resolve returns the per-account spawn env for sess, or nil for the
// system-default (account 0) binding. It degrades to nil on any resolver error
// so a materialize failure never blocks a spawn.
func (r *SpawnEnvResolver) Resolve(ctx context.Context, sess *models.Session) map[string]string {
	if r == nil || r.resolver == nil || sess == nil {
		return nil
	}
	env, err := r.resolver.ResolveSpawnEnv(ctx, deref(sess.AccountID), sess.AgentName, time.Now())
	if err != nil {
		r.log.Warn().Err(err).Str("agent", sess.AgentName).
			Msg("account: resolve spawn env failed; using system default")
		return nil
	}
	return env
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}
