package main

import "github.com/recurser/bossd/internal/serve"

func (w *daemonWiring) wireServe() {
	w.serveRunner = serve.New(w.sessions, w.repos, w.tmuxClient, w.displayTracker, serve.Config{DataDir: w.appDataDir, DaemonID: w.tmuxDaemonID, LoginShell: w.settings.LoginShell})
	w.lifecycle.SetServeStopper(w.serveRunner)
}
