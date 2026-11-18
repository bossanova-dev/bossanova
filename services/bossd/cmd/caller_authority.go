package main

import (
	"context"
	"os"

	"github.com/recurser/bossd/internal/adapter/peercred"
	"github.com/recurser/bossd/internal/adapter/tmux"
	"github.com/recurser/bossd/internal/callerauth"
)

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
