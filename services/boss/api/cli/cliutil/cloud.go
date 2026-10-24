package cliutil

import (
	"context"
	"errors"
	"fmt"
	"os"

	"github.com/recurser/boss/internal/adapter/cloudrpc"

	"github.com/recurser/boss/internal/auth"
	"github.com/spf13/cobra"
)

func CloudURL(cmd *cobra.Command) string {
	if cmd != nil && cmd.Root() != nil {
		if remote, err := cmd.Root().Flags().GetString("remote"); err == nil && remote != "" {
			return remote
		}
	}
	if cloud := os.Getenv("BOSS_CLOUD_URL"); cloud != "" {
		return cloud
	}
	if cloud, ok := os.LookupEnv("BOSSD_ORCHESTRATOR_URL"); ok {
		return cloud
	}
	return defaultCloudURL
}

func DialCloudRemote(ctx context.Context, mgr *auth.Manager, url string, notLoggedIn error) (*cloudrpc.RemoteClient, error) {
	remote, err := cloudRemote(ctx, mgr, url)
	if err != nil {
		return nil, fmt.Errorf("%w (%v)", notLoggedIn, err)
	}
	return remote, nil
}
func cloudRemote(ctx context.Context, mgr *auth.Manager, url string) (*cloudrpc.RemoteClient, error) {
	token, err := mgr.AccessToken(ctx)
	if err != nil {
		return nil, err
	}
	if token == "" {
		return nil, errors.New("not logged in")
	}
	return cloudrpc.NewRemote(url, token), nil
}

const defaultCloudURL = "https://orchestrator.bossanova.dev"
