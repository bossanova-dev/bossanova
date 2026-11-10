package testharness

import (
	"context"

	"github.com/recurser/bossd/api/upstream"
	"github.com/recurser/bossd/internal/db"
	"github.com/recurser/bossd/internal/domain"
)

type harnessWebhookTargets struct {
	repos    db.RepoStore
	sessions db.SessionStore
	chats    db.AgentChatStore
}

func (h harnessWebhookTargets) WebhookTargets(ctx context.Context, origin string, pr int) (string, []upstream.CommandTargetObject, error) {
	repo, err := h.repos.GetByOriginURL(ctx, origin)
	if err != nil {
		return "", nil, err
	}
	rows, err := h.sessions.ListWithRepo(ctx, repo.ID)
	if err != nil {
		return "", nil, err
	}
	var objects []upstream.CommandTargetObject
	for _, sess := range rows {
		if pr > 0 && (sess.PRNumber == nil || *sess.PRNumber != pr) {
			continue
		}
		objects = append(objects, upstream.CommandTargetObject{RepoID: sess.RepoID, SessionID: sess.ID})
		chats, err := h.chats.ListBySession(ctx, sess.ID)
		if err != nil {
			return "", nil, err
		}
		for _, chat := range chats {
			if chat.SessionID != sess.ID {
				return "", nil, domain.ErrCloudTarget
			}
			parent, err := h.sessions.Get(ctx, chat.SessionID)
			if err != nil {
				return "", nil, err
			}
			objects = append(objects, upstream.CommandTargetObject{RepoID: parent.RepoID, SessionID: chat.SessionID, ChatID: chat.AgentSessionID})
		}
	}
	return repo.ID, objects, nil
}
