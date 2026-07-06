package accountwiring

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/rs/zerolog"

	bossanovav1 "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossd/internal/agent"
	"github.com/recurser/bossd/internal/credmaterialize"
)

const (
	defaultSmokeTimeout      = 2 * time.Minute
	defaultSmokePollInterval = 500 * time.Millisecond
	smokePrompt              = "Reply with exactly: OK"
)

// SmokeCredentialStore is the credential store surface needed to materialize
// account credentials for a live smoke run and persist Codex refreshes back.
type SmokeCredentialStore interface {
	Load(accountID string) ([]byte, error)
	Save(accountID string, blob []byte) error
}

// SmokeRunnerOption configures a SmokeRunner.
type SmokeRunnerOption func(*smokeRunnerConfig)

type smokeRunnerConfig struct {
	baseDir      string
	timeout      time.Duration
	pollInterval time.Duration
}

// WithSmokeBaseDir overrides the credential-materialization base dir.
// Production leaves it unset; tests use a temp dir.
func WithSmokeBaseDir(dir string) SmokeRunnerOption {
	return func(c *smokeRunnerConfig) { c.baseDir = dir }
}

// WithSmokeTimeout overrides the live smoke timeout.
func WithSmokeTimeout(d time.Duration) SmokeRunnerOption {
	return func(c *smokeRunnerConfig) { c.timeout = d }
}

// WithSmokePollInterval overrides the ExitStatus polling interval.
func WithSmokePollInterval(d time.Duration) SmokeRunnerOption {
	return func(c *smokeRunnerConfig) { c.pollInterval = d }
}

// SmokeRunner runs a short live provider invocation using a registered account's
// materialized credentials.
type SmokeRunner struct {
	clients      map[string]agent.AgentRunnerClient
	materializer *credmaterialize.Materializer
	timeout      time.Duration
	pollInterval time.Duration
	logger       zerolog.Logger
}

// NewSmokeRunner builds a live account smoke runner. It materializes credentials
// using the same on-disk executor that spawn paths use for Codex CODEX_HOME.
func NewSmokeRunner(
	clients map[string]agent.AgentRunnerClient,
	store SmokeCredentialStore,
	logger zerolog.Logger,
	opts ...SmokeRunnerOption,
) (*SmokeRunner, error) {
	cfg := smokeRunnerConfig{timeout: defaultSmokeTimeout, pollInterval: defaultSmokePollInterval}
	for _, opt := range opts {
		opt(&cfg)
	}
	if cfg.timeout <= 0 {
		cfg.timeout = defaultSmokeTimeout
	}
	if cfg.pollInterval <= 0 {
		cfg.pollInterval = defaultSmokePollInterval
	}

	cmOpts := []credmaterialize.Option{}
	if cfg.baseDir != "" {
		cmOpts = append(cmOpts, credmaterialize.WithBaseDir(cfg.baseDir))
	}
	m, err := credmaterialize.New(credentialStoreAdapter{store: store}, logger, cmOpts...)
	if err != nil {
		return nil, err
	}
	return &SmokeRunner{
		clients:      clients,
		materializer: m,
		timeout:      cfg.timeout,
		pollInterval: cfg.pollInterval,
		logger:       logger,
	}, nil
}

// Smoke materializes accountID, starts a tiny headless run under provider, and
// waits for a clean exit. blob is accepted to satisfy the server hook; the
// materializer reloads the current keyring value by account id.
func (r *SmokeRunner) Smoke(ctx context.Context, accountID, provider string, _ []byte) error {
	if r == nil {
		return fmt.Errorf("account smoke runner not configured")
	}
	client := r.clients[provider]
	if client == nil {
		return fmt.Errorf("account smoke: no agent plugin client for provider %q", provider)
	}

	env, persist, err := r.materialize(ctx, accountID, provider)
	if err != nil {
		return err
	}

	workDir, err := os.MkdirTemp("", "boss-account-smoke-work-*")
	if err != nil {
		return fmt.Errorf("create smoke workdir: %w", err)
	}
	defer func() { _ = os.RemoveAll(workDir) }()
	logDir, err := os.MkdirTemp("", "boss-account-smoke-log-*")
	if err != nil {
		return fmt.Errorf("create smoke log dir: %w", err)
	}
	defer func() { _ = os.RemoveAll(logDir) }()

	sessionID, err := smokeSessionID()
	if err != nil {
		return err
	}
	if _, err := client.StartRun(ctx, &bossanovav1.StartAgentRunRequest{
		WorkDir:   workDir,
		Plan:      smokePrompt,
		SessionId: sessionID,
		LogPath:   filepath.Join(logDir, sessionID+".log"),
		ExtraEnv:  env,
	}); err != nil {
		return fmt.Errorf("account smoke start: %w", err)
	}

	if err := r.waitClean(ctx, client, sessionID); err != nil {
		_, _ = client.StopRun(context.Background(), &bossanovav1.StopAgentRunRequest{SessionId: sessionID})
		return err
	}
	if persist != nil {
		if err := persist(ctx); err != nil {
			return fmt.Errorf("account smoke persist refreshed credential: %w", err)
		}
	}
	return nil
}

func (r *SmokeRunner) materialize(ctx context.Context, accountID, provider string) (map[string]string, credmaterialize.PersistBack, error) {
	switch provider {
	case "claude":
		mat, err := r.materializer.MaterializeClaude(ctx, accountID)
		if err != nil {
			return nil, nil, fmt.Errorf("materialize claude account: %w", err)
		}
		return mat.Env, nil, nil
	case "codex":
		mat, persist, err := r.materializer.MaterializeCodex(ctx, accountID)
		if err != nil {
			return nil, nil, fmt.Errorf("materialize codex account: %w", err)
		}
		return mat.Env, persist, nil
	default:
		return nil, nil, fmt.Errorf("account smoke: unknown provider %q", provider)
	}
}

func (r *SmokeRunner) waitClean(ctx context.Context, client agent.AgentRunnerClient, sessionID string) error {
	wctx, cancel := context.WithTimeout(ctx, r.timeout)
	defer cancel()
	ticker := time.NewTicker(r.pollInterval)
	defer ticker.Stop()

	for {
		resp, err := client.ExitStatus(wctx, &bossanovav1.AgentExitStatusRequest{SessionId: sessionID})
		if err != nil {
			return fmt.Errorf("account smoke exit status: %w", err)
		}
		if resp.GetIsComplete() {
			if resp.GetExitError() != "" {
				return fmt.Errorf("account smoke failed: %s", resp.GetExitError())
			}
			return nil
		}
		select {
		case <-wctx.Done():
			return fmt.Errorf("account smoke timed out: %w", wctx.Err())
		case <-ticker.C:
		}
	}
}

func smokeSessionID() (string, error) {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("generate smoke session id: %w", err)
	}
	return "account-smoke-" + hex.EncodeToString(b[:]), nil
}

type credentialStoreAdapter struct {
	store SmokeCredentialStore
}

func (a credentialStoreAdapter) LoadCredential(_ context.Context, accountID string) ([]byte, error) {
	return a.store.Load(accountID)
}

func (a credentialStoreAdapter) SaveCredential(_ context.Context, accountID string, blob []byte) error {
	return a.store.Save(accountID, blob)
}
