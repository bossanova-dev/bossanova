package upstream

import (
	"context"
	"strings"
	"sync"

	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/kit/config"
	"github.com/recurser/bossalib/kit/daemoncaps"
	"github.com/rs/zerolog"
)

// CloudControlPolicy supplies the machine's local cloud command authority.
// A missing policy always means read-only.
type CloudControlPolicy interface {
	Mode() config.CloudControlMode
	RepoMode(context.Context, string) (config.CloudControlMode, config.CloudControlSource)
}

// RepoIdentities supplies identities verified using local credentials.
type RepoIdentities interface {
	Identity(context.Context, string) (config.RepoIdentity, error)
}

// LocalCloudControlPolicy reads the operator-owned settings file.
type LocalCloudControlPolicy struct {
	logger         zerolog.Logger
	RepoIdentities RepoIdentities
	grantOnce      sync.Once
	unknownOnce    sync.Once
	unreadableOnce sync.Once
}

// NewCloudControlPolicy reads settings afresh for each decision, so a local
// operator can grant or revoke control without restarting bossd.
func NewCloudControlPolicy(logger zerolog.Logger) *LocalCloudControlPolicy {
	return &LocalCloudControlPolicy{logger: logger}
}

func (p *LocalCloudControlPolicy) settings() (config.Settings, error) {
	settings, err := config.Load()
	if err != nil {
		p.unreadableOnce.Do(func() { p.logger.Warn().Msg("cloud control settings unreadable; using read-only") })
		return settings, err
	}
	raw := strings.TrimSpace(settings.CloudControl)
	if raw != "" && raw != "full" && raw != "read-only" && raw != "private" {
		p.unknownOnce.Do(func() { p.logger.Warn().Msg("unknown cloud control setting; using read-only") })
	}
	for _, grant := range settings.CloudControlRepos {
		if grant.Mode != "full" && grant.Mode != "read-only" && grant.Mode != "private" {
			p.grantOnce.Do(func() { p.logger.Warn().Msg("unparseable cloud control repo grant; using restrictive fallback") })
		}
	}
	return settings, nil
}

func (p *LocalCloudControlPolicy) Mode() config.CloudControlMode {
	settings, err := p.settings()
	if err != nil {
		return config.CloudControlReadOnly
	}
	return config.MachineCloudControl(settings)
}

func (p *LocalCloudControlPolicy) RepoMode(ctx context.Context, repoID string) (config.CloudControlMode, config.CloudControlSource) {
	settings, err := p.settings()
	if err != nil {
		return config.CloudControlReadOnly, config.CloudControlSourceDefault
	}
	var id config.RepoIdentity
	if len(settings.CloudControlRepos) > 0 && p.RepoIdentities != nil {
		verified, err := p.RepoIdentities.Identity(ctx, repoID)
		if err == nil {
			id = verified
		}
	}
	return config.ResolveRepoCloudControl(settings, id)
}

func cloudControlMode(policy CloudControlPolicy) config.CloudControlMode {
	if policy != nil {
		return config.ResolveCloudControl(string(policy.Mode()))
	}
	return config.CloudControlReadOnly
}

func cloudControlProto(policy CloudControlPolicy) pb.CloudControl {
	switch cloudControlMode(policy) {
	case config.CloudControlFull:
		return pb.CloudControl_CLOUD_CONTROL_FULL
	case config.CloudControlPrivate:
		return pb.CloudControl_CLOUD_CONTROL_PRIVATE
	default:
		return pb.CloudControl_CLOUD_CONTROL_READ_ONLY
	}
}

func (c *StreamClient) cloudControlCommandRefusal(mode pb.CloudControl, cmd *pb.OrchestratorCommand) string {
	access, classified := daemoncaps.OrchestratorCommandAccess(cmd)
	if mode == pb.CloudControl_CLOUD_CONTROL_PRIVATE && classified && access == daemoncaps.AccessObserve && daemoncaps.OrchestratorCommandCarriesContent(cmd) {
		return daemoncaps.CloudControlPrivateText(c.hostname)
	}
	return c.cloudControlRefusal()
}

func (c *StreamClient) cloudControlRefusal() string {
	return daemoncaps.CloudControlDisabledText(c.hostname)
}

// UnscopedMode protects cross-repository content even for a restrictive grant
// whose checkout is currently unregistered or unavailable.
func (p *LocalCloudControlPolicy) UnscopedMode() config.CloudControlMode {
	settings, err := p.settings()
	if err != nil {
		return config.CloudControlReadOnly
	}
	mode := config.MachineCloudControl(settings)
	for _, grant := range settings.CloudControlRepos {
		grantMode := config.ResolveCloudControl(grant.Mode)
		if grant.Mode != string(grantMode) {
			grantMode = config.CloudControlReadOnly
		}
		mode = restrictiveMode(mode, grantMode)
	}
	return mode
}
