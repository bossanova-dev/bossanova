package main

import (
	"github.com/recurser/bossalib/config"
	"github.com/recurser/bossd/internal/upstream"
)

// resolveDaemonIdentity fills in the two daemon-scoped fields on cfg, keeping
// them strictly separate:
//
//   - cfg.DaemonID is the ROUTING identity (BOSSD_DAEMON_ID, else the UUID
//     persisted under dataDir, else the machine hostname). It is resolved from
//     the real OS hostname captured before any display override is applied, so
//     renaming a daemon can never re-key it, rotate the persisted UUID, or
//     invalidate a stream token.
//   - cfg.Hostname is PRESENTATION metadata — the self-reported name bosso
//     shows in daemon listings. The operator's daemon_name setting overrides it;
//     blank keeps the machine hostname.
//
// A non-nil error means the persisted-id path failed and cfg.DaemonID holds the
// hostname fallback; callers should log it and carry on (bossd's startup does).
func resolveDaemonIdentity(cfg *upstream.Config, settings config.Settings, getenv func(string) string, dataDir string) error {
	return resolveDaemonIdentityWith(cfg, settings, getenv, dataDir, upstream.ResolveDaemonID)
}

// daemonIDResolver is upstream.ResolveDaemonID's shape.
type daemonIDResolver func(getenv func(string) string, dataDir, hostname string) (string, error)

// resolveDaemonIdentityWith is resolveDaemonIdentity with the id resolver
// injected. The seam exists purely so a test can assert WHICH hostname identity
// resolution was handed: ResolveDaemonID only lets the hostname reach the id on
// its last-resort fallback paths, so without this seam the ordering that the
// whole presentation-vs-identity contract rests on is only observable through
// that one narrow path. The mutation the seam catches is resolving the display
// name FIRST and handing that to resolveID; a test that pins the ordering any
// other way stays green under it.
func resolveDaemonIdentityWith(cfg *upstream.Config, settings config.Settings, getenv func(string) string, dataDir string, resolveID daemonIDResolver) error {
	if cfg == nil {
		return nil
	}
	// Capture the machine hostname BEFORE the display override lands on the
	// config — identity resolution must never see the override.
	machineHostname := cfg.Hostname

	daemonID, err := resolveID(getenv, dataDir, machineHostname)
	cfg.DaemonID = daemonID
	cfg.Hostname = config.DaemonDisplayName(settings, machineHostname)
	return err
}
