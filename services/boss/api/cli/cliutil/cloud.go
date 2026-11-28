package cliutil

import (
	"context"
	"errors"
	"fmt"
	"os"

	"github.com/recurser/boss/internal/adapter/cloudrpc"
	"github.com/recurser/boss/internal/auth"
	"github.com/recurser/bossalib/contract/cloud/cloudorigin"
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

// CloudToken returns the bearer token to send to url.
//
// When BOSS_CLOUD_TOKEN is non-empty, it is returned directly as an explicit
// credential for any URL without reading the keychain or refreshing. Otherwise
// the stored Bossanova Cloud login is loaded through mgr.AccessTokenFor, which
// refuses unknown origins and cross-environment tokens.
func CloudToken(ctx context.Context, mgr *auth.Manager, url string) (string, error) {
	if tok := os.Getenv("BOSS_CLOUD_TOKEN"); tok != "" {
		return tok, nil
	}
	if mgr == nil {
		return "", nil
	}
	token, err := mgr.AccessTokenFor(ctx, url)
	if isCloudOriginRefusal(err) {
		return "", fmt.Errorf("%w Set BOSS_CLOUD_TOKEN to a credential for that server", err)
	}
	return token, err
}

func isCloudOriginRefusal(err error) bool {
	return errors.Is(err, cloudorigin.ErrUnknownOrchestrator) || errors.Is(err, cloudorigin.ErrEnvironmentMismatch)
}

func DialCloudRemote(ctx context.Context, mgr *auth.Manager, url string, notLoggedIn error) (*cloudrpc.RemoteClient, error) {
	remote, err := cloudRemote(ctx, mgr, url)
	if err != nil {
		if isCloudOriginRefusal(err) {
			return nil, err
		}
		return nil, fmt.Errorf("%w (%v)", notLoggedIn, err)
	}
	return remote, nil
}

func cloudRemote(ctx context.Context, mgr *auth.Manager, url string) (*cloudrpc.RemoteClient, error) {
	token, err := CloudToken(ctx, mgr, url)
	if err != nil {
		return nil, err
	}
	if token == "" {
		return nil, errors.New("not logged in")
	}
	return cloudrpc.NewRemote(url, token), nil
}

const defaultCloudURL = "https://orchestrator.bossanova.dev"
