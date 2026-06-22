// Package trackerprompt renders an external tracker issue (Linear, Sentry)
// as the markdown "plan" prompt seeded into a new session. It is the single
// source of truth shared by the boss TUI (client-side) and the bossd daemon
// (server-side, for web-originated sessions).
package trackerprompt

import (
	"fmt"
	"strings"

	pb "github.com/recurser/bossalib/gen/bossanova/v1"
)

// Format renders the issue under a source-specific label ("Linear issue:" or
// "Sentry issue:"). Missing fields are individually omitted so we never render
// blank lines for absent data. A nil issue formats to "".
func Format(issue *pb.TrackerIssue, source string) string {
	if issue == nil {
		return ""
	}
	label := "Linear issue:"
	if source == "sentry" {
		label = "Sentry issue:"
	}
	var b strings.Builder
	b.WriteString(label)
	b.WriteString("\n\n")
	header := issue.Title
	if issue.ExternalId != "" {
		if header != "" {
			header = fmt.Sprintf("[%s] %s", issue.ExternalId, issue.Title)
		} else {
			header = fmt.Sprintf("[%s]", issue.ExternalId)
		}
	}
	if header != "" {
		b.WriteString(header)
		b.WriteString("\n")
	}
	if desc := strings.TrimSpace(issue.Description); desc != "" {
		b.WriteString("\n")
		b.WriteString(desc)
		b.WriteString("\n")
	}
	if issue.Url != "" {
		b.WriteString("\n")
		b.WriteString(issue.Url)
		b.WriteString("\n")
	}
	return b.String()
}
