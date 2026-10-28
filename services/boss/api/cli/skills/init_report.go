package skills

import (
	"fmt"
	"strings"
)

func (iv *initInterview) summarizeTracker(line func(string, string)) {
	s := iv.steps
	if iv.opts.tracker == "github" {
		line("Tracker", "GitHub Issues "+s.Overrides.Repository)
		line("gh login", s.GitHubViewer)
	} else {
		switch {
		case s.LinearStored:
			text := s.linearText()
			if s.LinearVerified {
				text += "; validated (" + viewerDisplay(iv.viewer) + ")"
			}
			line("Linear key", text)
		case iv.linearKey == "":
			line("Linear key", "skipped")
		case iv.repo != nil && iv.linearKey == iv.storedKey:
			line("Linear key", "already stored, validated ("+viewerDisplay(iv.viewer)+")")
		default:
			line("Linear key", "validated, used for this run only (not stored)")
		}
	}
}

func writeTrackerReport(b *strings.Builder, cfg detectedConfig, tracker initTracker) {
	written := cfg.TrackerConfig != nil
	github := written && cfg.TrackerConfig.GitHub != nil
	if github {
		fmt.Fprintf(b, "Tracker: GitHub Issues %s — gh login %s\n", cfg.TrackerConfig.GitHub.Repository, tracker.viewer)
	}
	if written && !github {
		line := "Tracker team: " + cfg.TrackerConfig.Linear.Team
		if cfg.TrackerConfig.Linear.TeamKey != "" {
			line += " (" + cfg.TrackerConfig.Linear.TeamKey + ")"
		}
		if tracker.source != "" {
			line += " — " + tracker.source
		}
		b.WriteString(line + "\n")
		if tracker.note != "" {
			b.WriteString("  " + tracker.note + "\n")
		}
	}

}
