package diagnostics

import (
	"context"
	"fmt"
	"os"
	"time"

	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/spf13/cobra"
)

// GitHubReadCredentialClient fetches a credential from the local daemon only.
// Implementations must never log its response, which can contain a secret.
type GitHubReadCredentialClient interface {
	GetGitHubReadCredential(context.Context, *pb.GetGitHubReadCredentialRequest) (*pb.GetGitHubReadCredentialResponse, error)
}

func (deps *Group) ghReadCredentialCmd() *cobra.Command {
	return &cobra.Command{
		Use: "__gh-read-credential", Hidden: true,
		DisableFlagParsing: true,
		// Avoid interactive installation, transport selection and command telemetry
		// in the root startup hook. The gh wrapper must never prompt or read stdin.
		PersistentPreRunE: func(*cobra.Command, []string) error { return nil },
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) > 0 && args[0] == "--" {
				args = args[1:]
			}
			if deps.gitHubReadCredentialClient == nil {
				return nil
			}
			ctx, cancel := context.WithTimeout(cmd.Context(), 5*time.Second)
			defer cancel()
			cmd.SetContext(ctx)
			wd, err := os.Getwd()
			if err != nil {
				return nil
			}
			client, err := deps.gitHubReadCredentialClient(cmd)
			if err != nil || client == nil {
				return nil
			}
			response, err := client.GetGitHubReadCredential(ctx, &pb.GetGitHubReadCredentialRequest{GhArgs: args, WorkingDirectory: wd, GhHost: deps.osGetenv("GH_HOST"), GhRepo: deps.osGetenv("GH_REPO")})
			if err == nil && response.GetToken() != "" {
				_, _ = fmt.Fprintf(cmd.OutOrStdout(), "%d %s\n", response.GetInstallationId(), response.GetToken())
			}
			return nil
		},
	}
}
