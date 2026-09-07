// This file contains the platform-independent supervision-mode resolver.
package daemon

import (
	"errors"
	"fmt"
	"strings"
)

// SupervisionMode names the supervision substrate an operator has CONFIGURED
// for bossd. It is a static choice read out of settings.json — an answer that
// exists on a host where nothing is installed and nothing is running.
//
// That is what separates it from the three runtime-observation concepts this
// package and its callers already carry, none of which it may be conflated
// with:
//
//   - ServingMode (servingmode.go, BOS-1181) observes what is CURRENTLY
//     serving the socket: unserved / supervised / standalone.
//   - SpawnState (spawnhistory.go, BOS-1183) observes what the service
//     manager's spawn HISTORY says: never-spawned / failing / healthy.
//   - daemonSupervisionVerdict (services/boss/cmd/daemon_supervision.go,
//     BOS-1183) observes who OWNS the live daemon right now.
//
// All three answer "what is true of this machine". SupervisionMode answers
// "which substrate was asked for", so a change to it can never move any of
// those three verdicts and none of them can move it. Reading this type as a
// statement about a running daemon is a category error: an operator can select
// a mode the machine has never once run under.
//
// It is macOS-only. Linux's substrate is a systemd user unit, which with
// lingering enabled already survives the user being logged out entirely, so
// there is nothing to select there — see supervisionmode_other.go.
type SupervisionMode string

const (
	// SupervisionModeLaunchAgent is today's substrate and the DEFAULT: the
	// per-user LaunchAgent at ~/Library/LaunchAgents/com.bossanova.bossd.plist,
	// bootstrapped into the gui/<uid> Aqua domain. An absent or empty settings
	// key resolves here, so a fresh settings.json and every settings.json
	// written before BOS-1184 keep today's behaviour unchanged.
	//
	// Its limitation is the whole reason this type exists: a backgrounded Aqua
	// domain keeps already-running services alive but refuses NEW spawns, so on
	// a host where this user is routinely backgrounded by fast user switching,
	// a crash respawn, a `boss daemon restart` and a reboot all fail to bring
	// bossd back.
	SupervisionModeLaunchAgent SupervisionMode = "launch-agent"

	// SupervisionModeUnattended is the multi-user-host substrate: supervision
	// that does not depend on the daemon's user owning the foreground console.
	//
	// It is a root-owned LaunchDaemon (WatchdogLabel) in the `system` domain
	// that spawns bossd through `launchctl asuser <uid>` and then drops
	// credentials with `sudo -u <user>`. The `system` domain has no
	// foreground-console concept, so RunAtLoad and KeepAlive both work on a
	// host where the daemon's user is backgrounded — which is precisely what
	// SupervisionModeLaunchAgent cannot do. See watchdog.go for the artifact
	// and docs/ops/daemon-supervision-modes.md for the operator-facing posture.
	//
	// Selecting it does NOT silently fall back to SupervisionModeLaunchAgent
	// when it cannot be honoured: the install and uninstall paths need root and
	// refuse with ErrUnattendedRequiresRoot, and any path a non-root user could
	// write is refused with ErrUnattendedInsecurePath. A fallback would hand a
	// multi-user host exactly the substrate it selected this mode to escape,
	// and report success while doing it.
	SupervisionModeUnattended SupervisionMode = "unattended"
)

// recognisedSupervisionModes is the single source both the resolver and its
// rejection message read, so a mode added to one cannot be missing from the
// other — an operator told "not one of [launch-agent]" about a value the
// resolver does accept would be worse than no message at all.
var recognisedSupervisionModes = []SupervisionMode{
	SupervisionModeLaunchAgent,
	SupervisionModeUnattended,
}

var (
	// ErrUnknownSupervisionMode is returned for a settings value that names no
	// recognised mode. Resolution FAILS CLOSED here rather than falling back to
	// the default: a typo must not silently select a substrate the operator did
	// not ask for on a host they configured precisely because the default does
	// not work there. This is deliberately the opposite direction from
	// config.parseSubagentDispatchGrant, which fails open because its default is
	// the more permissive answer; here the default is the answer that fails to
	// come back up.
	ErrUnknownSupervisionMode = errors.New("unrecognised daemon supervision mode")

	// ErrSupervisionModeUnsupportedPlatform is returned when a non-default mode
	// is selected off macOS. It exists so a mode value can never reach a
	// systemd code path: BOS-1184's scope is macOS, and Linux user units with
	// lingering already solve the problem this whole seam is about.
	ErrSupervisionModeUnsupportedPlatform = errors.New("daemon supervision mode is macOS-only")
)

// SupervisionModeStatus is the resolved supervision-substrate fact, gathered
// once and rendered by every surface that reports it.
//
// It carries the raw configured value alongside the verdict because the two
// answer different questions — "what did the operator write" and "what will
// bossd do" — and a reporting surface that had only the second could not tell
// an operator that the value they can see in settings.json is inert on this
// host.
type SupervisionModeStatus struct {
	// Configured is the raw settings value, trimmed of surrounding whitespace.
	// Empty means the key was absent, whitespace-only, or unreadable.
	Configured string
	// Mode is the substrate that will be used. It is empty when Err is set:
	// a rejected configuration resolves to NO mode, never to the default, so a
	// caller that ignores Err cannot accidentally act on a usable value.
	Mode SupervisionMode
	// Configurable reports whether this platform selects a supervision
	// substrate at all. False off macOS, where the systemd user unit is the
	// only substrate and this key is inert.
	Configurable bool
	// Err is the fail-closed rejection, or nil. It wraps either
	// ErrUnknownSupervisionMode or ErrSupervisionModeUnsupportedPlatform.
	Err error
	// Unattended is the observation of what the unattended substrate looks like
	// on this host, gathered only when Mode resolves to
	// SupervisionModeUnattended and left at UnattendedInstallNotApplicable
	// otherwise.
	//
	// It sits on the CONFIGURATION status because the two facts are useless
	// apart: "unattended is selected" plus "nothing is installed" describes a
	// host with no supervision at all, and a surface that could see only the
	// first would report the mode affirmatively while the daemon had no way
	// back up. Gathering it lazily is what keeps the default path unchanged —
	// a host that has not set the key performs no filesystem observation at
	// all.
	Unattended UnattendedInstall
	// SettingsErr is the reason the settings file could not be read, or nil.
	//
	// It is deliberately NOT Err: Mode still carries the default (see
	// LoadSupervisionModeStatus), so the install path is unaffected. What it
	// exists for is the reporting surfaces — without it they would print an
	// affirmative "launch-agent (the default)" for a file that names nothing,
	// which tells an operator whose settings.json is corrupt that the mode they
	// wrote is in effect. Naming the fallback rather than discarding the reason
	// is the same shape config.LoadSubagentDispatchGrant uses.
	SettingsErr error
}

// LoadSupervisionModeStatus reads the configured supervision mode from settings
// and resolves it.
//
// A settings file that cannot be read resolves to the DEFAULT rather than to an
// error. That direction looks like it contradicts the fail-closed rule above,
// and does not: fail-closed governs a value the operator wrote, while an
// unreadable settings file means no value was written at all. Every other
// consumer of these settings already behaves this way (serviceEnvSettings), and
// making an unreadable settings.json newly break `boss daemon install` would
// change default-path behaviour, which BOS-1184 R5 forbids. Note that config.Load
// returns defaults and NO error for an absent file, so this branch means a file
// that exists and could not be read or parsed — never a fresh host.
//
// The reason is carried out in SettingsErr rather than discarded, though.
// Resolving to the default is the right substrate decision; reporting it as
// though the operator had configured it is not, and the two surfaces that render
// this status would otherwise tell an operator with a corrupt settings.json that
// their mode is in effect.
func LoadSupervisionModeStatus() SupervisionModeStatus {
	settings, err := loadServiceSettings()
	if err != nil {
		return SupervisionModeStatus{
			Mode:         SupervisionModeLaunchAgent,
			Configurable: supervisionModeConfigurable,
			SettingsErr:  err,
		}
	}
	raw := strings.TrimSpace(settings.DaemonSupervisionMode)
	mode, resolveErr := ResolveSupervisionMode(raw)
	status := SupervisionModeStatus{
		Configured:   raw,
		Mode:         mode,
		Configurable: supervisionModeConfigurable,
		Err:          resolveErr,
	}
	if resolveErr == nil && mode == SupervisionModeUnattended {
		status.Unattended = observeUnattendedInstall()
	}
	return status
}

// ResolveSupervisionMode maps a raw settings value to the substrate bossd will
// use, or returns the reason it refuses to pick one.
//
// The two halves are separated on purpose. parseSupervisionMode answers "is
// this a mode at all", which is platform-independent; supervisionModeAvailability
// answers "can this machine act on it", which is not. Keeping the second pure —
// it takes the platform fact as an argument rather than reading the build-tagged
// const itself — is what lets both platforms' behaviour be proven from either
// one.
//
// This function is therefore only the wiring: the composition itself lives in
// resolveSupervisionMode below, which takes that platform fact too, and this
// exported form supplies the const. That is the whole shape servingmode.go uses
// — the pure ClassifyServingMode(ServingFacts) with StandaloneServingSupported()
// as a separate accessor — and adopting only its first half is what previously
// forced supervisionmode_test.go to keep a second copy of this composition just
// to reach the other platform's posture.
func ResolveSupervisionMode(raw string) (SupervisionMode, error) {
	return resolveSupervisionMode(raw, supervisionModeConfigurable)
}

// resolveSupervisionMode is ResolveSupervisionMode with the platform fact
// supplied rather than read from the build-tagged const, so both platforms'
// behaviour is provable from either one's test run WITHOUT a test-side copy of
// this composition. Tests drive this; production drives the wrapper above.
func resolveSupervisionMode(raw string, configurable bool) (SupervisionMode, error) {
	mode, err := parseSupervisionMode(raw)
	if err != nil {
		return "", err
	}
	if err := supervisionModeAvailability(mode, configurable); err != nil {
		return "", err
	}
	return mode, nil
}

// parseSupervisionMode maps a raw settings value onto a recognised mode.
//
// Empty and whitespace-only resolve to the default, so an absent key is
// byte-identical to today. Case and surrounding whitespace are normalised, so a
// plausible spelling ("Unattended", " unattended ") is honoured rather than
// rejected on a technicality — but an UNRECOGNISED value is refused outright
// and no mode is returned with it.
func parseSupervisionMode(raw string) (SupervisionMode, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return SupervisionModeLaunchAgent, nil
	}
	candidate := SupervisionMode(strings.ToLower(trimmed))
	for _, mode := range recognisedSupervisionModes {
		if candidate == mode {
			return mode, nil
		}
	}
	return "", fmt.Errorf("%w: %q is not one of %s", ErrUnknownSupervisionMode, trimmed, supervisionModeList())
}

// supervisionModeAvailability reports why a recognised mode cannot be acted on,
// or nil when it can. configurable is the platform fact — true where a
// supervision substrate is selectable at all — passed in rather than read from
// the build-tagged const so this whole matrix is testable on either platform.
func supervisionModeAvailability(mode SupervisionMode, configurable bool) error {
	if mode == SupervisionModeLaunchAgent {
		// The default is always available: off macOS it is the token meaning
		// "today's substrate", and nothing on the systemd path ever reads it.
		return nil
	}
	if !configurable {
		return fmt.Errorf(
			"%w: %q is selectable on macOS only — a systemd user unit with lingering enabled "+
				"('loginctl enable-linger') already survives its user being logged out",
			ErrSupervisionModeUnsupportedPlatform, mode)
	}
	// Every recognised mode is implemented on a platform that can select one.
	// There is deliberately no "recognised but not built yet" rung left here:
	// BOS-1184 U3 replaced it with the watchdog LaunchDaemon, and a refusal
	// nothing can reach is a message no operator could ever act on. What CAN
	// still refuse the unattended mode is its install path (root, and
	// root-owned paths) — reported as an observation through
	// SupervisionModeStatus.Unattended, not as a resolution failure, because
	// "the operator selected this substrate" and "this host has it installed"
	// are different facts.
	return nil
}

// supervisionModeList renders the recognised values for a rejection message.
func supervisionModeList() string {
	quoted := make([]string, 0, len(recognisedSupervisionModes))
	for _, mode := range recognisedSupervisionModes {
		quoted = append(quoted, fmt.Sprintf("%q", mode))
	}
	return "[" + strings.Join(quoted, ", ") + "]"
}
