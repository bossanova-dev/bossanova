package server

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"fmt"

	"connectrpc.com/connect"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bosso/internal/auth"
	"github.com/recurser/bosso/internal/db"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// CreateWebhookConfig registers a webhook configuration for a repo.
// Requires user auth (OIDC JWT).
func (s *Server) CreateWebhookConfig(ctx context.Context, req *connect.Request[pb.CreateWebhookConfigRequest]) (*connect.Response[pb.CreateWebhookConfigResponse], error) {
	info := auth.InfoFromContext(ctx)
	if info == nil || !info.IsUser() {
		return nil, connect.NewError(connect.CodeUnauthenticated, fmt.Errorf("user authentication required"))
	}

	msg := req.Msg
	if msg.RepoOriginUrl == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("repo_origin_url is required"))
	}
	if msg.Provider == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("provider is required"))
	}

	secret := msg.Secret
	if secret == "" {
		b := make([]byte, 32)
		if _, err := rand.Read(b); err != nil {
			return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("generate secret: %w", err))
		}
		secret = hex.EncodeToString(b)
	}

	config, err := s.webhooks.Create(ctx, db.CreateWebhookConfigParams{
		RepoOriginURL: msg.RepoOriginUrl,
		Provider:      msg.Provider,
		Secret:        secret,
	})
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("create webhook config: %w", err))
	}

	return connect.NewResponse(&pb.CreateWebhookConfigResponse{
		Config: webhookConfigToProto(config),
	}), nil
}

// ListWebhookConfigs returns all webhook configurations.
// Requires user auth (OIDC JWT).
func (s *Server) ListWebhookConfigs(ctx context.Context, _ *connect.Request[pb.ListWebhookConfigsRequest]) (*connect.Response[pb.ListWebhookConfigsResponse], error) {
	info := auth.InfoFromContext(ctx)
	if info == nil || !info.IsUser() {
		return nil, connect.NewError(connect.CodeUnauthenticated, fmt.Errorf("user authentication required"))
	}

	configs, err := s.webhooks.List(ctx)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("list webhook configs: %w", err))
	}

	pbConfigs := make([]*pb.WebhookConfig, len(configs))
	for i, c := range configs {
		pbConfigs[i] = webhookConfigToProto(c)
	}

	return connect.NewResponse(&pb.ListWebhookConfigsResponse{
		Configs: pbConfigs,
	}), nil
}

// DeleteWebhookConfig removes a webhook configuration.
// Requires user auth (OIDC JWT).
func (s *Server) DeleteWebhookConfig(ctx context.Context, req *connect.Request[pb.DeleteWebhookConfigRequest]) (*connect.Response[pb.DeleteWebhookConfigResponse], error) {
	info := auth.InfoFromContext(ctx)
	if info == nil || !info.IsUser() {
		return nil, connect.NewError(connect.CodeUnauthenticated, fmt.Errorf("user authentication required"))
	}

	if req.Msg.Id == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("id is required"))
	}

	if err := s.webhooks.Delete(ctx, req.Msg.Id); err != nil {
		if err == sql.ErrNoRows {
			return nil, connect.NewError(connect.CodeNotFound, fmt.Errorf("webhook config not found"))
		}
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("delete webhook config: %w", err))
	}

	return connect.NewResponse(&pb.DeleteWebhookConfigResponse{}), nil
}

func webhookConfigToProto(c *db.WebhookConfig) *pb.WebhookConfig {
	return &pb.WebhookConfig{
		Id:            c.ID,
		RepoOriginUrl: c.RepoOriginURL,
		Provider:      c.Provider,
		Secret:        c.Secret,
		CreatedAt:     timestamppb.New(c.CreatedAt),
	}
}
