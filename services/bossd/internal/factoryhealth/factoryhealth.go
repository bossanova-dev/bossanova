// Package factoryhealth derives public factory-health problems from local facts.
package factoryhealth

import (
	"fmt"
	"regexp"
	"slices"
	"sort"
	"strings"
	"time"
)

const (
	// BacklogThreshold is the time a Verifying head can wait before being reported.
	BacklogThreshold = time.Hour
	// MaxProblems is the public proof record's health-list limit.
	MaxProblems  = 20
	maxTextChars = 300
)

// Problem describes one degraded fact; a zero StartedAt means unknown.
type Problem struct {
	Code, Text string
	StartedAt  time.Time
}

// Note is a host-local second-model health note.
type Note struct {
	Tags      []string
	Body      string
	CreatedAt time.Time
}

// CronFact is an enabled verify job with its already-classified last outcome.
type CronFact struct {
	Name        string
	LastRunAt   *time.Time
	LastOutcome string
	Failing     bool
}

// VerifyingHead is a tracked PR head currently waiting for verification.
type VerifyingHead struct {
	PR        int
	StartedAt time.Time
}

// Facts contains already-read inputs; unreadable sources must be named explicitly.
type Facts struct {
	SecondModelNotes                    []Note
	VerifyCrons                         []CronFact
	VerifyEnabled, PublishingConfigured bool
	Verifying                           []VerifyingHead
	Unreadable                          []string
	DaemonVersion, SkillsVersion        string
	DaemonOutdated                      bool
}

var codeOrder = []string{"second-model-off", "verify-cron-failing", "publishing-off", "verify-backlog", "daemon-outdated", "health-partial"}
var notePattern = regexp.MustCompile(`^second-model review unavailable: ([^ ]+) ([^ ]+) since ([^, ]+)`)
var recoveredPattern = regexp.MustCompile(`^second-model review recovered: ([^ ]+) ran again on host `)
var safeToken = regexp.MustCompile(`^[a-z0-9_.-]{1,40}$`)

// Compute returns deterministic, bounded problems in public code order.
func Compute(f Facts, now time.Time) []Problem {
	problems := secondModelProblems(f.SecondModelNotes)
	for _, job := range f.VerifyCrons {
		if !job.Failing {
			continue
		}
		started := time.Time{}
		if job.LastRunAt != nil {
			started = *job.LastRunAt
		}
		date := "unknown"
		if !started.IsZero() {
			date = started.UTC().Format(time.DateOnly)
		}
		problems = append(problems, Problem{Code: "verify-cron-failing", Text: fmt.Sprintf("Verify cron %q failing (last run %s: %s)", truncate(job.Name, 80), date, job.LastOutcome), StartedAt: started})
	}
	if f.VerifyEnabled && !f.PublishingConfigured {
		problems = append(problems, Problem{Code: "publishing-off", Text: "Evidence publishing off: no upload credential on this host"})
	}
	if p, ok := backlogProblem(f.Verifying, now); ok {
		problems = append(problems, p)
	}
	if f.DaemonOutdated {
		problems = append(problems, Problem{Code: "daemon-outdated", Text: fmt.Sprintf("Daemon %s is older than installed skills %s", f.DaemonVersion, f.SkillsVersion)})
	}
	if len(f.Unreadable) > 0 {
		problems = append(problems, Problem{Code: "health-partial", Text: "Factory health partly unreadable: " + strings.Join(f.Unreadable, ", ")})
	}
	rank := make(map[string]int, len(codeOrder))
	for i, code := range codeOrder {
		rank[code] = i
	}
	sort.SliceStable(problems, func(i, j int) bool { return rank[problems[i].Code] < rank[problems[j].Code] })
	if len(problems) > MaxProblems {
		problems = problems[:MaxProblems]
	}
	for i := range problems {
		problems[i].Text = truncate(problems[i].Text, maxTextChars)
	}
	return problems
}

type parsedNote struct {
	note                  Note
	agent, classification string
	since                 time.Time
}

func parseNote(note Note) parsedNote {
	p := parsedNote{note: note, since: note.CreatedAt}
	match := notePattern.FindStringSubmatch(note.Body)
	if match == nil {
		recovered := recoveredPattern.FindStringSubmatch(note.Body)
		if recovered != nil && safeToken.MatchString(recovered[1]) {
			p.agent = recovered[1]
		}
		return p
	}
	if safeToken.MatchString(match[1]) {
		p.agent = match[1]
	}
	if safeToken.MatchString(match[2]) {
		p.classification = match[2]
	}
	if since, err := time.Parse(time.RFC3339, match[3]); err == nil {
		p.since = since
	}
	return p
}

func secondModelProblems(notes []Note) []Problem {
	newest := make(map[string]parsedNote)
	for _, note := range notes {
		p := parseNote(note)
		previous, ok := newest[p.agent]
		if !ok || note.CreatedAt.After(previous.note.CreatedAt) || note.CreatedAt.Equal(previous.note.CreatedAt) && slices.Contains(note.Tags, "recovered") {
			newest[p.agent] = p
		}
	}
	agents := make([]string, 0, len(newest))
	for agent := range newest {
		agents = append(agents, agent)
	}
	sort.Strings(agents)
	var problems []Problem
	for _, agent := range agents {
		p := newest[agent]
		if slices.Contains(p.note.Tags, "recovered") {
			continue
		}
		text := "Second model"
		if agent != "" {
			text += " (" + agent + ")"
		}
		text += " not running since " + p.since.UTC().Format(time.DateOnly)
		if p.classification != "" {
			text += " (" + p.classification + ")"
		}
		problems = append(problems, Problem{Code: "second-model-off", Text: text, StartedAt: p.since})
	}
	return problems
}

func backlogProblem(heads []VerifyingHead, now time.Time) (Problem, bool) {
	var oldest VerifyingHead
	count := 0
	for _, head := range heads {
		if head.StartedAt.IsZero() || !head.StartedAt.Before(now.Add(-BacklogThreshold)) {
			continue
		}
		count++
		if oldest.StartedAt.IsZero() || head.StartedAt.Before(oldest.StartedAt) || head.StartedAt.Equal(oldest.StartedAt) && head.PR < oldest.PR {
			oldest = head
		}
	}
	if count == 0 {
		return Problem{}, false
	}
	return Problem{Code: "verify-backlog", Text: fmt.Sprintf("%d PR(s) waiting for verify over an hour (oldest #%d)", count, oldest.PR), StartedAt: oldest.StartedAt}, true
}

func truncate(text string, limit int) string {
	chars := []rune(text)
	if len(chars) <= limit {
		return text
	}
	return string(chars[:limit-1]) + "…"
}
