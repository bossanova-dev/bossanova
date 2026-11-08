package main

import (
	"context"
	"os"

	"github.com/recurser/bossalib/kit/config"
	"github.com/recurser/bossd/internal/adapter/peercred"
	"github.com/recurser/bossd/internal/adapter/tmux"
	"github.com/recurser/bossd/internal/callerauth"
	"github.com/recurser/bossd/internal/domain"
	"github.com/rs/zerolog"
)

// callerGrants resolves the socket gate's role grants from settings, read once
// at startup: the coordinator approval grant unless the operator opted out.
func callerGrants(settings *config.Settings, logger zerolog.Logger) domain.CallerGrants {
	mode, discarded := config.ResolveCoordinatorApprovalGrant(settings)
	if discarded != "" {
		logger.Warn().Str("coordinator_approval_grant", string(mode)).Msg(discarded)
	}
	if mode != config.CoordinatorApprovalGrantOn {
		logger.Info().Msg("coordinator approval grant withheld: no chat role may approve or merge")
		return nil
	}
	logger.Info().Msg("coordinator approval grant on: coordinator chats may approve and merge")
	return domain.CoordinatorApprovalGrants()
}

func (w *daemonWiring) callerAuthority() *callerauth.Authority {
	return callerauth.New(callerauth.Config{
		PeerPID: peercred.PeerPID, ParentPID: peercred.ParentPID, DaemonPID: os.Getpid(),
		Chats: w.agentChats.ListWithTmuxSession,
		PaneSessions: func(ctx context.Context) (map[int][]string, error) {
			panes, err := w.tmuxClient.ListPanes(ctx)
			if err != nil {
				return nil, err
			}
			return callerPaneSessions(panes), nil
		},
	})
}

func callerPaneSessions(panes []tmux.Pane) map[int][]string {
	out := make(map[int][]string, len(panes))
	for _, pane := range panes {
		out[pane.PID] = append(out[pane.PID], pane.SessionName)
	}
	return out
}
