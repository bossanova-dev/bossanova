package home

import (
	"github.com/recurser/boss/api/tui/ui"
	"github.com/recurser/boss/internal/agent"
	"github.com/recurser/boss/internal/browser"
)

type AuthChangeQueue = ui.AuthChangeQueue

var NewAuthChangeQueue = ui.NewAuthChangeQueue
var OpenURLFunc = browser.OpenURL
var AgentChatTitle = agent.ChatTitle
