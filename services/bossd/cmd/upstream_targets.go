package main

import (
	"context"
	"errors"

	"github.com/recurser/bossalib/kit/config"
	"github.com/recurser/bossd/api/upstream"
	"github.com/recurser/bossd/internal/db"
	"github.com/recurser/bossd/internal/domain"
	"github.com/recurser/bossd/internal/repoidentity"
)

// upstreamTargets keeps store dependencies at the cmd boundary. The upstream
// door receives only ownership projections, without content or credentials.
type upstreamTargets struct {
	repos      db.RepoStore
	sessions   db.SessionStore
	chats      db.AgentChatStore
	cronJobs   db.CronJobStore
	notes      db.NoteStore
	callbacks  db.GitHubCallbackStore
	identities *repoidentity.Verifier
}

func (a upstreamTargets) Identity(ctx context.Context, id string) (config.RepoIdentity, error) {
	repo, err := a.repos.Get(ctx, id)
	if err != nil {
		return config.RepoIdentity{}, err
	}
	return a.identities.Identity(ctx, *repo), nil
}

func (a upstreamTargets) ListTargetRepos(ctx context.Context) ([]upstream.CommandTargetObject, error) {
	repos, err := a.repos.List(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]upstream.CommandTargetObject, 0, len(repos))
	for _, repo := range repos {
		out = append(out, upstream.CommandTargetObject{RepoID: repo.ID, Name: repo.DisplayName})
	}
	return out, nil
}

func (a upstreamTargets) GetTarget(ctx context.Context, kind, id string) (upstream.CommandTargetObject, error) {
	var object upstream.CommandTargetObject
	switch kind {
	case "repo":
		repo, err := a.repos.Get(ctx, id)
		if err != nil {
			return object, err
		}
		return upstream.CommandTargetObject{RepoID: repo.ID, Name: repo.DisplayName}, nil
	case "repo-name", "trigger":
		origin := id
		if kind == "repo-name" {
			origin = "https://github.com/" + id
		}
		repo, err := a.repos.GetByOriginURL(ctx, origin)
		if err != nil {
			return object, err
		}
		return upstream.CommandTargetObject{RepoID: repo.ID, Name: repo.DisplayName}, nil
	case "session", "transfer":
		session, err := a.sessions.Get(ctx, id)
		if err != nil {
			return object, err
		}
		return upstream.CommandTargetObject{RepoID: session.RepoID}, nil
	case "chat":
		chat, err := a.chats.GetByAgentSessionID(ctx, id)
		if err != nil {
			return object, err
		}
		session, err := a.sessions.Get(ctx, chat.SessionID)
		if err != nil {
			return object, domain.ErrCloudTarget
		}
		return upstream.CommandTargetObject{RepoID: session.RepoID, SessionID: chat.SessionID}, nil
	case "cron":
		job, err := a.cronJobs.Get(ctx, id)
		if err != nil {
			return object, err
		}
		return upstream.CommandTargetObject{RepoID: job.RepoID}, nil
	case "note":
		note, err := a.notes.Get(ctx, id)
		if err != nil {
			return object, err
		}
		object.RepoID = note.RepoID
		if note.SessionID != nil {
			object.SessionID = *note.SessionID
		}
		if note.ChatID != nil {
			object.ChatID = *note.ChatID
		}
		return object, nil
	case "callback":
		callback, err := a.callbacks.Get(ctx, id)
		if err != nil {
			return object, err
		}
		repo, err := a.repos.GetByOriginURL(ctx, "https://github.com/"+callback.RepoOwner+"/"+callback.RepoName)
		if err != nil {
			return object, err
		}
		return upstream.CommandTargetObject{RepoID: repo.ID, ChatID: callback.TargetChatID}, nil
	default:
		return object, errors.New("unknown command target kind")
	}
}

var _ upstream.CommandTargetObjects = upstreamTargets{}
var _ upstream.RepoIdentities = upstreamTargets{}

// WebhookTargets resolves the envelope against local registrations, including
// every chat whose session may receive an event for that PR.
func (a upstreamTargets) WebhookTargets(ctx context.Context, origin string, pr int) (string, []upstream.CommandTargetObject, error) {
	repo, err := a.repos.GetByOriginURL(ctx, origin)
	if err != nil {
		return "", nil, err
	}
	sessions, err := a.sessions.ListWithRepo(ctx, repo.ID)
	if err != nil {
		return "", nil, err
	}
	var objects []upstream.CommandTargetObject
	for _, sess := range sessions {
		if pr > 0 && (sess.PRNumber == nil || *sess.PRNumber != pr) {
			continue
		}
		objects = append(objects, upstream.CommandTargetObject{RepoID: sess.RepoID, SessionID: sess.ID})
		chats, err := a.chats.ListBySession(ctx, sess.ID)
		if err != nil {
			return "", nil, err
		}
		for _, chat := range chats {
			if chat.SessionID != sess.ID {
				return "", nil, domain.ErrCloudTarget
			}
			object, err := a.GetTarget(ctx, "chat", chat.AgentSessionID)
			if err != nil {
				return "", nil, err
			}
			objects = append(objects, object)
		}
	}
	return repo.ID, objects, nil
}
