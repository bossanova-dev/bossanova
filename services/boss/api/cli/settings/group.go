package settings

import (
	"context"
	"fmt"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"

	daemonrpc "github.com/recurser/boss/internal/adapter/daemonrpc"
	"github.com/recurser/boss/internal/auth"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/kit/config"
	"github.com/recurser/bossalib/kit/telemetry"
	"github.com/spf13/cobra"
)

// RepoCloudControlReader supplies locally verified registered repository identities.
type RepoCloudControlReader interface {
	ListRepoCloudControls(context.Context) ([]*pb.RepoCloudControl, error)
}

// Options supplies command telemetry and the daemon and browser boundaries.
// Each group owns its dependencies, so constructing another root cannot change them.
type Options struct {
	NewRepoCloudControlReader   func(string) RepoCloudControlReader
	TelemetryClient             func(*cobra.Command) telemetry.Client
	CaptureAuthChangedWithEmail func(context.Context, telemetry.Client, string, string)
	TelemetryEnabled            func() bool
	DistinctID                  func() string
	DaemonAuthSocketPath        func() (string, error)
	NewDaemonAuthNotifier       func(string) DaemonAuthNotifier
	OpenCloudCheckoutURL        func(string) error
}

type Group struct{ options Options }

func New(options Options) *Group {
	if options.TelemetryClient == nil {
		options.TelemetryClient = func(*cobra.Command) telemetry.Client { return nil }
	}
	if options.CaptureAuthChangedWithEmail == nil {
		options.CaptureAuthChangedWithEmail = func(context.Context, telemetry.Client, string, string) {}
	}
	if options.TelemetryEnabled == nil {
		options.TelemetryEnabled = func() bool { return false }
	}
	if options.DistinctID == nil {
		options.DistinctID = func() string { return "" }
	}
	if options.DaemonAuthSocketPath == nil {
		options.DaemonAuthSocketPath = daemonrpc.DefaultSocketPath
	}
	if options.NewDaemonAuthNotifier == nil {
		options.NewDaemonAuthNotifier = func(socket string) DaemonAuthNotifier { return daemonrpc.NewLocal(socket) }
	}
	if options.OpenCloudCheckoutURL == nil {
		options.OpenCloudCheckoutURL = auth.OpenBrowser
	}
	if options.NewRepoCloudControlReader == nil {
		options.NewRepoCloudControlReader = func(socket string) RepoCloudControlReader { return daemonrpc.NewLocal(socket) }
	}
	return &Group{options: options}
}

func (g *Group) repoCloudControls(ctx context.Context) ([]*pb.RepoCloudControl, error) {
	socket, err := g.options.DaemonAuthSocketPath()
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	return g.options.NewRepoCloudControlReader(socket).ListRepoCloudControls(ctx)
}

func repoCloudControlIdentity(repo *pb.RepoCloudControl) config.RepoIdentity {
	return config.RepoIdentity{Host: repo.GetHost(), Name: repo.GetName(), LocalPath: repo.GetCheckout(), ProviderRepoID: repo.GetProviderRepoId(), IsIDVerified: repo.GetIsIdVerified()}
}

func repoCloudControlOwner(repo *pb.RepoCloudControl) string {
	owner, _, ok := strings.Cut(repo.GetName(), "/")
	if !ok || owner == "" {
		owner = "(unknown owner)"
	}
	host := repo.GetHost()
	if host == "" {
		host = "local"
	}
	return host + "/" + owner
}

func repoCloudControlMatches(repo *pb.RepoCloudControl, selector string) bool {
	if selector == repo.GetOriginUrl() || selector == repo.GetDisplayName() || selector == repo.GetName() || selector == repo.GetCheckout() {
		return true
	}
	path, err := filepath.Abs(selector)
	return err == nil && filepath.IsAbs(repo.GetCheckout()) && filepath.Clean(path) == filepath.Clean(repo.GetCheckout())
}

func (g *Group) applyRepoCloudControlFlags(cmd *cobra.Command, s *config.Settings) (bool, error) {
	if !cmd.Flags().Changed("cloud-control-repo") && !cmd.Flags().Changed("cloud-control-owner") {
		return false, nil
	}
	repos, err := g.repoCloudControls(cmd.Context())
	if err != nil {
		return false, fmt.Errorf("list registered repos: %w", err)
	}
	full := false
	for _, flag := range []string{"cloud-control-owner", "cloud-control-repo"} {
		values, _ := cmd.Flags().GetStringArray(flag)
		for _, value := range values {
			changedFull, err := applyRepoCloudControlValue(s, repos, flag, value)
			if err != nil {
				return false, err
			}
			full = full || changedFull
		}
	}
	return full, nil
}

func applyRepoCloudControlValue(s *config.Settings, repos []*pb.RepoCloudControl, flag, value string) (bool, error) {
	selector, mode, ok := strings.Cut(value, "=")
	if !ok || selector == "" || mode == "" {
		return false, fmt.Errorf("%s requires <repo or host/owner>=<default|private|read-only|full>", flag)
	}
	matches := matchingRepoCloudControls(repos, flag, selector)
	if len(matches) == 0 {
		return false, fmt.Errorf("no registered repo matches %q", selector)
	}
	if flag == "cloud-control-repo" && len(matches) != 1 {
		return false, fmt.Errorf("repo %q is ambiguous; use a checkout path", selector)
	}
	for _, repo := range matches {
		if err := config.SetRepoCloudControl(s, repoCloudControlIdentity(repo), mode); err != nil {
			return false, err
		}
	}
	return mode == "full", nil
}

func matchingRepoCloudControls(repos []*pb.RepoCloudControl, flag, selector string) []*pb.RepoCloudControl {
	var matches []*pb.RepoCloudControl
	for _, repo := range repos {
		if repo == nil {
			continue
		}
		if flag == "cloud-control-owner" {
			if strings.Contains(selector, "/") && strings.EqualFold(selector, repoCloudControlOwner(repo)) {
				matches = append(matches, repo)
			}
		} else if repoCloudControlMatches(repo, selector) {
			matches = append(matches, repo)
		}
	}
	return matches
}

func (g *Group) printRepoCloudControls(cmd *cobra.Command) {
	repos, err := g.repoCloudControls(cmd.Context())
	fmt.Println("  Cloud control per repo:")
	if err != nil {
		_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "Registered repo cloud control unavailable: %v\n", err)
		return
	}
	repos = slices.Clone(repos)
	sort.SliceStable(repos, func(i, j int) bool {
		left, right := repoCloudControlOwner(repos[i]), repoCloudControlOwner(repos[j])
		if left != right {
			return left < right
		}
		return repos[i].GetDisplayName() < repos[j].GetDisplayName()
	})
	group := ""
	for _, repo := range repos {
		if repo == nil {
			continue
		}
		owner := repoCloudControlOwner(repo)
		if owner != group {
			fmt.Printf("    %s\n", owner)
			group = owner
		}
		name := repo.GetDisplayName()
		if name == "" {
			name = repo.GetName()
		}
		warning := ""
		if repo.GetHasUnverifiedGrant() {
			warning = " (grant not verified: narrowing only)"
		}
		fmt.Printf("      %s: %s (%s)%s\n", name, repo.GetEffectiveMode(), repo.GetSource(), warning)
	}
}
