package skills

import "strings"

const skillRemedyUnverifiedNote = "reinstall direction unverified: no checkout history to compare the installed copy against"

const skillRemedyWithheldLead = "reinstall withheld [lost] — it would overwrite installed content this checkout cannot restore:"

// skillRemedyDowngradeLead is the withheld lead when every withheld path is
// newer than this checkout rather than lost to it: the content is restorable
// from origin/HEAD, but a reinstall from here would replace it with an older
// revision. It keeps the "reinstall withheld" prefix consumers key on.
const skillRemedyDowngradeLead = "reinstall withheld [downgrade] — the installed copy is newer than this checkout (it is on origin/HEAD); a reinstall from here would downgrade it:"

// skillRemedyUndecidedLead is the withheld lead for a safety decision that
// could not be computed at all. Failing to read the installed tree while
// deciding whether a reinstall destroys it is not evidence that it does not,
// so it withholds in the same direction as a known loss.
const skillRemedyUndecidedLead = "reinstall withheld [undecided] — the installed-vs-checkout comparison failed, so whether a reinstall would overwrite unrecoverable content is undecided"

const skillRemedyNextActionDestroys = "next: set the listed paths aside and reinstall with"

const skillRemedyNextActionUndecided = "next: re-run this check once the skills directory is readable and no longer changing underneath it"

const skillRemedyNextActionDowngrade = "next: update this checkout to origin/HEAD, then re-run this check"

// skillRemedyAdvice is a composed reinstall prescription: either a runnable
// command or an explicit refusal naming what the command would have destroyed.
type skillRemedyAdvice struct {
	command       string
	setAside      string
	unverified    bool
	refusal       skillRemedyRefusal
	withheldPaths []skillDriftEntry
}

type skillRemedyRefusal int

const (
	skillRefusalNone skillRemedyRefusal = iota
	skillRefusalLost
	skillRefusalDowngrade
	skillRefusalUndecided
)

func (r skillRemedyRefusal) String() string {
	switch r {
	case skillRefusalNone:
		return "none"
	case skillRefusalLost:
		return "lost"
	case skillRefusalDowngrade:
		return "downgrade"
	case skillRefusalUndecided:
		return "undecided"
	default:
		return "undecided"
	}
}

func (d skillDriftDirection) refusal() skillRemedyRefusal {
	switch d {
	case skillDriftLossless, skillDriftBehind:
		return skillRefusalNone
	case skillDriftAhead:
		return skillRefusalDowngrade
	case skillDriftUnrecoverable, skillDriftUnknown:
		return skillRefusalLost
	default:
		return skillRefusalLost
	}
}

func (a skillRemedyAdvice) withheld() bool {
	return a.refusal != skillRefusalNone
}

// withheldLead is the headline for a refusal, naming why it refused.
func (a skillRemedyAdvice) withheldLead() string {
	switch a.refusal {
	case skillRefusalNone:
		return ""
	case skillRefusalLost:
		return skillRemedyWithheldLead
	case skillRefusalDowngrade:
		return skillRemedyDowngradeLead
	case skillRefusalUndecided:
		return skillRemedyUndecidedLead
	default:
		return skillRemedyUndecidedLead
	}
}

// nextAction is what the operator does about a refusal.
func (a skillRemedyAdvice) nextAction() string {
	switch a.refusal {
	case skillRefusalNone:
		return ""
	case skillRefusalLost:
		return skillRemedyNextActionDestroys + " `" + a.setAside + "` (copies them to the boss skill-backups directory first; nothing is deleted)"
	case skillRefusalDowngrade:
		return skillRemedyNextActionDowngrade
	case skillRefusalUndecided:
		return skillRemedyNextActionUndecided
	default:
		return skillRemedyNextActionUndecided
	}
}

// composeSkillInstallRemedy is the single decision point every drift surface
// uses, so the surfaces cannot drift apart in their safety behaviour.
//
// overwrites reports whether an installed tree exists for the reinstall to
// write over; ancestry reports whether this checkout has history to consult.
// Without ancestry the direction is stated as unverified rather than silently
// assumed correct; with it, any path that is not a lossless forward fix
// withholds the plain reinstall and offers preservation for known loss.
func composeSkillInstallRemedy(payload selectedSkillPayload, entries []skillDriftEntry, ancestry, overwrites bool, agent ...string) skillRemedyAdvice {
	command := skillInstallRemedy(payload)
	if !overwrites {
		return skillRemedyAdvice{command: command}
	}
	if !ancestry {
		return skillRemedyAdvice{command: command, unverified: true}
	}
	setAside := command + " --set-aside"
	if len(agent) > 0 && agent[0] != "" {
		setAside += " --agent " + agent[0]
	}
	advice := skillRemedyAdvice{command: command, setAside: setAside}
	for _, entry := range entries {
		refusal := entry.direction.refusal()
		if refusal == skillRefusalNone {
			continue
		}
		advice.withheldPaths = append(advice.withheldPaths, entry)
		// A genuine loss anywhere keeps the destructive refusal, regardless of order.
		if advice.refusal == skillRefusalNone || refusal == skillRefusalLost {
			advice.refusal = refusal
		}
	}
	return advice
}

func (a skillRemedyAdvice) runLine() string {
	line := "run `" + a.command + "`"
	if a.unverified {
		line += " (" + skillRemedyUnverifiedNote + ")"
	}
	return line
}

// inline renders the advice as a clause appended to a one-line drift warning.
func (a skillRemedyAdvice) inline() string {
	if !a.withheld() {
		return a.runLine()
	}
	if len(a.withheldPaths) == 0 {
		return a.withheldLead() + "; " + a.nextAction()
	}
	paths := make([]string, 0, len(a.withheldPaths))
	for _, entry := range a.withheldPaths {
		paths = append(paths, entry.label())
	}
	return a.withheldLead() + " " + strings.Join(paths, ", ") + "; " + a.nextAction()
}

// lines renders the advice as indented report lines under a drift verdict.
func (a skillRemedyAdvice) lines(indent string) []string {
	if !a.withheld() {
		return []string{indent + a.runLine()}
	}
	return []string{indent + a.withheldLead(), indent + a.nextAction()}
}
