package skillinstall

import (
	"errors"
	"fmt"
	"io/fs"
	"os/exec"
)

// AgentCLI pairs a supported coding agent with its display name and CLI binary.
type AgentCLI struct {
	Agent   Agent
	Name    string
	Command string
}

// AgentCLIs returns the supported coding agents in canonical order.
func AgentCLIs() []AgentCLI {
	return []AgentCLI{
		{Agent: AgentClaude, Name: "Claude", Command: "claude"},
		{Agent: AgentCodex, Name: "Codex", Command: "codex"},
		{Agent: AgentHermes, Name: "Hermes", Command: "hermes"},
		{Agent: AgentAntigravity, Name: "Antigravity", Command: "agy"},
	}
}

// Declines carries per-agent, per-manifest decline state supplied by callers
// so this package stays independent of kit/config. The legacy Claude-only
// SkillsDeclined bit is intentionally omitted.
type Declines struct {
	ByAgent         map[string]bool
	ManifestByAgent map[string]string
}

// Declined reports whether agent was explicitly declined for this exact
// payload manifest.
func (d Declines) Declined(agent Agent, manifest string) bool {
	key := string(agent)
	return d.ByAgent[key] && d.ManifestByAgent[key] == manifest
}

// ConsentAction is the decision taken for one agent during a consent install.
type ConsentAction string

const (
	// ConsentInstall installs the embedded payload into an absent agent tree.
	ConsentInstall ConsentAction = "install"
	// ConsentPresent leaves an already-installed tree untouched.
	ConsentPresent ConsentAction = "present"
	// ConsentNoCLI skips an agent whose CLI is not on PATH.
	ConsentNoCLI ConsentAction = "no-cli"
	// ConsentDeclined skips an agent declined for the same payload manifest.
	ConsentDeclined ConsentAction = "declined"
	// ConsentSkippedEnv skips when BOSS_SKIP_SKILLS is set.
	ConsentSkippedEnv ConsentAction = "skipped-env"
	// ConsentSettingsUnreadable fails closed when settings.json cannot be read.
	ConsentSettingsUnreadable ConsentAction = "settings-unreadable"
)

// ConsentFacts holds the inputs to DecideConsentInstall for one agent.
type ConsentFacts struct {
	SkipEnv          bool
	Installed        bool
	CLIFound         bool
	SettingsReadable bool
	Declined         bool
}

// DecideConsentInstall applies the consent-install decision table in priority
// order (first match wins).
func DecideConsentInstall(f ConsentFacts) ConsentAction {
	switch {
	case f.SkipEnv:
		return ConsentSkippedEnv
	case f.Installed:
		return ConsentPresent
	case !f.CLIFound:
		return ConsentNoCLI
	case !f.SettingsReadable:
		return ConsentSettingsUnreadable
	case f.Declined:
		return ConsentDeclined
	default:
		return ConsentInstall
	}
}

// ConsentEnv supplies the payload, decline state and seams used by
// InstallOnConsent and RefreshInstalled.
type ConsentEnv struct {
	FS               fs.FS
	Record           PayloadRecord
	Manifest         string
	Declines         Declines
	SettingsReadable bool
	SkipEnv          bool
	LookPath         func(string) (string, error)
	DirFor           func(Agent) (string, error)
}

// ConsentOutcome reports the consent-install decision and result for one agent.
type ConsentOutcome struct {
	Agent     Agent
	Action    ConsentAction
	Installed bool
	Err       error
}

// RefreshOutcome reports the upgrade-refresh result for one agent.
type RefreshOutcome struct {
	Agent     Agent
	Updated   bool
	Restamped bool
	Held      bool
	Decision  RefreshDecision
	Err       error
}

// InstallIfAbsent fresh-installs fsys into dir, stamping record, only if no
// boss skill tree is present once the per-directory update lock is held.
// If another writer installed a tree before the lock was acquired, it leaves
// that tree untouched and returns (false, nil).
func InstallIfAbsent(dir string, fsys fs.FS, record PayloadRecord) (installed bool, err error) {
	if fsys == nil {
		return false, errors.New("nil skill payload filesystem")
	}
	lock, err := acquireUpdateLock(dir)
	if err != nil {
		return false, err
	}
	defer func() {
		if unlockErr := lock.Unlock(); err == nil && unlockErr != nil {
			installed = false
			err = fmt.Errorf("release skill update lock: %w", unlockErr)
		}
	}()
	if IsInstalled(dir) {
		return false, nil
	}
	if err := extract(dir, fsys, &record); err != nil {
		return false, err
	}
	return true, nil
}

// InstallOnConsent walks AgentCLIs(), decides each agent with
// DecideConsentInstall, and calls InstallIfAbsent for each ConsentInstall.
func InstallOnConsent(env ConsentEnv) []ConsentOutcome {
	agents := AgentCLIs()
	outcomes := make([]ConsentOutcome, 0, len(agents))
	if env.SkipEnv {
		for _, target := range agents {
			outcomes = append(outcomes, ConsentOutcome{
				Agent:  target.Agent,
				Action: ConsentSkippedEnv,
			})
		}
		return outcomes
	}
	lookPath := env.LookPath
	if lookPath == nil {
		lookPath = exec.LookPath
	}
	dirFor := env.DirFor
	if dirFor == nil {
		dirFor = DirForAgent
	}
	for _, target := range agents {
		outcomes = append(outcomes, consentInstallAgent(target, dirFor, lookPath, env))
	}
	return outcomes
}

func consentInstallAgent(
	target AgentCLI,
	dirFor func(Agent) (string, error),
	lookPath func(string) (string, error),
	env ConsentEnv,
) ConsentOutcome {
	dir, err := dirFor(target.Agent)
	if err != nil {
		return ConsentOutcome{Agent: target.Agent, Err: err}
	}
	installed := IsInstalled(dir)
	cliFound := false
	if !installed {
		_, lookErr := lookPath(target.Command)
		cliFound = lookErr == nil
	}
	action := DecideConsentInstall(ConsentFacts{
		SkipEnv:          env.SkipEnv,
		Installed:        installed,
		CLIFound:         cliFound,
		SettingsReadable: env.SettingsReadable,
		Declined:         env.Declines.Declined(target.Agent, env.Manifest),
	})
	if action != ConsentInstall {
		return ConsentOutcome{Agent: target.Agent, Action: action}
	}
	didInstall, err := InstallIfAbsent(dir, env.FS, env.Record)
	if err != nil {
		return ConsentOutcome{Agent: target.Agent, Action: action, Err: err}
	}
	if !didInstall {
		return ConsentOutcome{Agent: target.Agent, Action: ConsentPresent}
	}
	return ConsentOutcome{
		Agent:     target.Agent,
		Action:    ConsentInstall,
		Installed: true,
	}
}

// RefreshInstalled runs the guarded upgrade refresh over every installed agent
// tree, skipping trees already stamped by this exact build and restamping
// matching trees whose previous record is not vouched.
func RefreshInstalled(env ConsentEnv) []RefreshOutcome {
	agents := AgentCLIs()
	outcomes := make([]RefreshOutcome, 0, len(agents))
	if env.SkipEnv {
		for _, target := range agents {
			outcomes = append(outcomes, RefreshOutcome{Agent: target.Agent})
		}
		return outcomes
	}
	dirFor := env.DirFor
	if dirFor == nil {
		dirFor = DirForAgent
	}
	for _, target := range agents {
		outcomes = append(outcomes, refreshInstalledAgent(target.Agent, dirFor, env))
	}
	return outcomes
}

// beforeRefreshLock is a package-internal test hook invoked after the unlocked
// ReadPayloadRecord fast path and before taking the update lock.
var beforeRefreshLock func()

func refreshInstalledAgent(
	agent Agent,
	dirFor func(Agent) (string, error),
	env ConsentEnv,
) RefreshOutcome {
	outcome := RefreshOutcome{Agent: agent}
	dir, err := dirFor(agent)
	if err != nil {
		outcome.Err = err
		return outcome
	}
	if !IsInstalled(dir) {
		return outcome
	}
	if env.SettingsReadable && env.Declines.Declined(agent, env.Manifest) {
		return outcome
	}
	existing, recorded, err := ReadPayloadRecord(dir)
	if err != nil {
		outcome.Err = err
		return outcome
	}
	if recorded && sameBuild(existing, env.Record) {
		return outcome
	}
	if env.FS == nil {
		outcome.Err = errors.New("nil skill payload filesystem")
		return outcome
	}
	if beforeRefreshLock != nil {
		beforeRefreshLock()
	}
	return refreshInstalledLocked(agent, dir, env.FS, env.Record)
}

// refreshInstalledLocked re-reads the on-disk record and either restamps a
// matching tree or applies the guarded refresh under one hold of the update
// lock, so a concurrent vouched or newer writer cannot be clobbered.
func refreshInstalledLocked(agent Agent, dir string, fsys fs.FS, record PayloadRecord) (outcome RefreshOutcome) {
	outcome.Agent = agent
	lock, err := acquireUpdateLock(dir)
	if err != nil {
		outcome.Err = err
		return outcome
	}
	defer func() {
		if unlockErr := lock.Unlock(); outcome.Err == nil && unlockErr != nil {
			outcome = RefreshOutcome{Agent: agent, Err: fmt.Errorf("release skill update lock: %w", unlockErr)}
		}
	}()
	if !IsInstalled(dir) {
		return outcome
	}
	existing, recorded, err := ReadPayloadRecord(dir)
	if err != nil {
		outcome.Err = err
		return outcome
	}
	if recorded && sameBuild(existing, record) {
		return outcome
	}
	needs, err := needsUpdateLocked(dir, fsys)
	if err != nil {
		outcome.Err = err
		return outcome
	}
	if !needs {
		if !shouldRestamp(existing, recorded, record) {
			return outcome
		}
		if recorded && existing == record {
			return outcome
		}
		if err := writePayloadRecord(dir, record); err != nil {
			outcome.Err = err
			return outcome
		}
		outcome.Restamped = true
		return outcome
	}
	decision := DecideRefresh(existing, recorded, record, ReleaseVersionOrder)
	outcome.Decision = decision
	if !decision.Refresh {
		outcome.Held = true
		return outcome
	}
	stamp := guardedStamp(record, decision)
	if err := extract(dir, fsys, &stamp); err != nil {
		outcome.Err = err
		return outcome
	}
	outcome.Updated = true
	return outcome
}

func shouldRestamp(existing PayloadRecord, recorded bool, record PayloadRecord) bool {
	if !record.IsRelease() {
		return false
	}
	if !recorded {
		return true
	}
	if existing.vouched() {
		return false
	}
	return ReleaseVersionOrder(existing, record) != OrderOlder
}
