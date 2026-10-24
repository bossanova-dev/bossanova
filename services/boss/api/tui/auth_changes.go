package tui

import "github.com/recurser/boss/api/tui/ui"

type authChangeQueue = ui.AuthChangeQueue

func newAuthChangeQueue() *authChangeQueue { return ui.NewAuthChangeQueue() }
