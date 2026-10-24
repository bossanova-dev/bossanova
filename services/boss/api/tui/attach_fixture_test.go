package tui

import (
	"context"

	"github.com/recurser/boss/api/tui/attach"
	"github.com/recurser/boss/internal/attachprocess"
	"github.com/recurser/boss/internal/pty"
)

func NewAttachModel(c attach.Client, ctx context.Context, manager *pty.Manager, sessionID, resumeID string) attach.Model {
	return attach.NewModel(c, ctx, manager, sessionID, resumeID, attachprocess.OS{})
}
