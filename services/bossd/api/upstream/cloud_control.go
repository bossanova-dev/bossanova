package upstream

import (
	"fmt"
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
}

// LocalCloudControlPolicy reads the operator-owned settings file.
type LocalCloudControlPolicy struct {
	logger         zerolog.Logger
	unknownOnce    sync.Once
	unreadableOnce sync.Once
}

// NewCloudControlPolicy reads settings afresh for each decision, so a local
// operator can grant or revoke control without restarting bossd.
func NewCloudControlPolicy(logger zerolog.Logger) *LocalCloudControlPolicy {
	return &LocalCloudControlPolicy{logger: logger}
}

func (p *LocalCloudControlPolicy) Mode() config.CloudControlMode {
	settings, err := config.Load()
	if err != nil {
		p.unreadableOnce.Do(func() { p.logger.Warn().Msg("cloud control settings unreadable; using read-only") })
		return config.CloudControlReadOnly
	}
	raw := strings.TrimSpace(settings.CloudControl)
	if raw != "" && raw != string(config.CloudControlReadOnly) && raw != string(config.CloudControlFull) && raw != string(config.CloudControlPrivate) {
		p.unknownOnce.Do(func() { p.logger.Warn().Msg("unknown cloud control setting; using read-only") })
	}
	return config.ResolveCloudControl(raw)
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
	return fmt.Sprintf("cloud control is read-only on %s; set `cloud_control` to `full` in that machine's local settings.json to allow it", c.hostname)
}
