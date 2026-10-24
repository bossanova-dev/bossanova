package home

import (
	"context"

	tea "charm.land/bubbletea/v2"
	"github.com/recurser/boss/api/tui/ui"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
)

// attentionActionCmd supplies the router's browser opener to shared attention actions.
func attentionActionCmd(ctx context.Context, c ui.AttentionClient, s *pb.Session, action pb.AttentionAction) tea.Cmd {
	return ui.AttentionActionCmd(ctx, c, s, action, OpenURLFunc)
}
