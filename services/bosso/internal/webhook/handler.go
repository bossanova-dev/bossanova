package webhook

import (
	"context"
	"io"
	"net/http"

	"connectrpc.com/connect"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/gen/bossanova/v1/bossanovav1connect"
	"github.com/recurser/bosso/internal/db"
	"github.com/rs/zerolog"
)

// DaemonPool provides access to daemon ConnectRPC clients.
type DaemonPool interface {
	Get(daemonID string) bossanovav1connect.DaemonServiceClient
}

// Handler is an HTTP handler for incoming VCS webhooks.
// It verifies signatures, parses events, and routes them to daemons.
type Handler struct {
	configs  db.WebhookConfigStore
	daemons  db.DaemonStore
	pool     DaemonPool
	registry *Registry
	logger   zerolog.Logger
}

// NewHandler creates a new webhook HTTP handler.
func NewHandler(
	configs db.WebhookConfigStore,
	daemons db.DaemonStore,
	pool DaemonPool,
	registry *Registry,
	logger zerolog.Logger,
) *Handler {
	return &Handler{
		configs:  configs,
		daemons:  daemons,
		pool:     pool,
		registry: registry,
		logger:   logger,
	}
}

// ServeHTTP handles incoming webhook requests.
// The URL path encodes the provider: /webhooks/github, /webhooks/gitlab, etc.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// Extract provider from path: /webhooks/{provider}
	provider := r.PathValue("provider")
	if provider == "" {
		http.Error(w, "missing provider in path", http.StatusBadRequest)
		return
	}

	parser, err := h.registry.Get(provider)
	if err != nil {
		http.Error(w, "unsupported provider", http.StatusBadRequest)
		return
	}

	// Read body (limit to 10MB to prevent abuse).
	body, err := io.ReadAll(io.LimitReader(r.Body, 10<<20))
	if err != nil {
		http.Error(w, "failed to read body", http.StatusBadRequest)
		return
	}

	// Parse first to get the repo URL, then verify signature.
	parsed, err := parser.Parse(r, body)
	if err != nil {
		h.logger.Warn().Err(err).Str("provider", provider).Msg("webhook parse error")
		http.Error(w, "parse error", http.StatusBadRequest)
		return
	}

	if parsed == nil {
		// Event type not relevant — acknowledge and ignore.
		w.WriteHeader(http.StatusOK)
		return
	}

	// Look up the webhook config for signature verification.
	config, err := h.configs.GetByRepo(r.Context(), parsed.RepoOriginURL, provider)
	if err != nil {
		h.logger.Warn().Err(err).
			Str("repo", parsed.RepoOriginURL).
			Str("provider", provider).
			Msg("no webhook config found")
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	// Verify signature.
	if err := parser.VerifySignature(r, body, config.Secret); err != nil {
		h.logger.Warn().Err(err).
			Str("repo", parsed.RepoOriginURL).
			Str("provider", provider).
			Msg("webhook signature verification failed")
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	// Route the event to daemons that manage this repo.
	delivered := h.deliverEvent(r.Context(), parsed)

	h.logger.Info().
		Str("repo", parsed.RepoOriginURL).
		Str("provider", provider).
		Int("daemons_delivered", delivered).
		Msg("webhook event processed")

	w.WriteHeader(http.StatusOK)
}

// deliverEvent sends the parsed VCS event to all daemons managing the repo.
// Returns the number of daemons the event was delivered to.
func (h *Handler) deliverEvent(ctx context.Context, parsed *ParsedEvent) int {
	// Find all daemons that manage this repo by iterating through all daemons
	// and checking their repo IDs. The daemon_repos table maps daemon IDs to
	// repo origin URLs via the daemon's registered repo IDs.
	//
	// For now, we look up daemons by matching repo origin URL in daemon_repos.
	// A future optimization would add an index or store to look up daemons by repo URL.
	delivered := 0

	// Get all daemons (across all users) that have this repo.
	daemons, err := h.findDaemonsByRepoURL(ctx, parsed.RepoOriginURL)
	if err != nil {
		h.logger.Error().Err(err).Str("repo", parsed.RepoOriginURL).Msg("failed to find daemons for repo")
		return 0
	}

	for _, daemon := range daemons {
		if !daemon.Online {
			continue
		}

		client := h.pool.Get(daemon.ID)
		if client == nil {
			continue
		}

		_, err := client.DeliverVCSEvent(ctx, connect.NewRequest(&pb.DeliverVCSEventRequest{
			RepoOriginUrl: parsed.RepoOriginURL,
			Event:         parsed.Event,
		}))
		if err != nil {
			h.logger.Warn().Err(err).
				Str("daemon", daemon.ID).
				Str("repo", parsed.RepoOriginURL).
				Msg("failed to deliver VCS event to daemon")
			continue
		}

		delivered++
	}

	return delivered
}

// findDaemonsByRepoURL finds all online daemons that manage a repo matching
// the given origin URL. This checks the daemon_repos join table.
func (h *Handler) findDaemonsByRepoURL(ctx context.Context, repoOriginURL string) ([]*db.Daemon, error) {
	// We need a way to find daemons by repo URL. Since daemon_repos stores
	// repo IDs (not URLs), and the orchestrator doesn't have the URL→ID mapping,
	// we use the repo origin URL as the repo ID in the daemon_repos table.
	// This works because daemons register with their repo origin URLs as IDs.
	//
	// Use a dedicated query method for this lookup.
	return h.daemons.ListByRepoID(ctx, repoOriginURL)
}
