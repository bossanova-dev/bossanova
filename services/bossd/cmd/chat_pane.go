package main

import (
	server "github.com/recurser/bossd/api/daemon"
	"github.com/recurser/bossd/api/upstream"
	"github.com/recurser/bossd/internal/adapter/tmux"
)

// The chat tmux client also reads a chat's question menu and sends its
// answer, which Server.AnswerChatQuestion finds on the client it was given;
// the stream's command adapter finds the answer and stop handlers on the
// server it holds as its Commands.
var (
	_ server.ChatPane            = (*tmux.Client)(nil)
	_ upstream.ChatAgentCommands = (*server.Server)(nil)
)
