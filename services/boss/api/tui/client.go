package tui

import (
	"context"

	"github.com/recurser/boss/api/tui/accounts"
	"github.com/recurser/boss/api/tui/attach"
	"github.com/recurser/boss/api/tui/bugreport"
	"github.com/recurser/boss/api/tui/chatpicker"
	"github.com/recurser/boss/api/tui/cron"
	"github.com/recurser/boss/api/tui/home"
	"github.com/recurser/boss/api/tui/login"
	"github.com/recurser/boss/api/tui/newsession"
	"github.com/recurser/boss/api/tui/repos"
	"github.com/recurser/boss/api/tui/settings"
	"github.com/recurser/boss/api/tui/trash"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
)

// Client composes the contracts of the screens constructed by App.
// Each screen continues to accept its own smaller interface.
type Client interface {
	accounts.ListClient
	accounts.EditClient
	accounts.RegisterClient
	attach.Client
	bugreport.Client
	chatpicker.Client
	cron.Client
	home.Client
	login.Client
	newsession.Client
	repos.AddClient
	repos.ListClient
	repos.SettingsClient
	settings.GeneralClient
	settings.SessionClient
	trash.Client
	ListRepos(context.Context) ([]*pb.Repo, error)
}
