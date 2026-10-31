// Package chatlabel formats stored automation titles and session-local display labels.
package chatlabel

import (
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"
)

// Role is the purpose of a chat.
type Role string

const (
	RoleBuild    Role = "build"
	RoleVerify   Role = "verify"
	RoleRepair   Role = "repair"
	RoleOperator Role = "operator"
)

// Title returns the short stored title for an automation chat.
func Title(role Role, reason string) string {
	title := string(role)
	if role == RoleRepair && reason != "" {
		title += ": " + reason
	}
	return title
}

// Chat is the title and ordering information needed to label a session's chats.
type Chat struct {
	ID        string
	Role      Role
	Title     string
	CreatedAt time.Time
}

var resumedSuffix = regexp.MustCompile(` \(resumed [0-9]+\)$`)
var numberedRole = regexp.MustCompile(`(?i)^(build|verify|repair)( [0-9]+)?$`)
var repairReason = regexp.MustCompile(`^repair(?: [0-9]+)?: (.*)$`)

// Labels computes labels by creation time, preserving input order for ties without
// mutating chats. Every chat of a role counts, including explicitly renamed and
// stopped chats. Deleting a chat can shift the ordinals of later chats.
func Labels(chats []Chat, sessionTitle string) map[string]string {
	ordered := slices.Clone(chats)
	slices.SortStableFunc(ordered, func(a, b Chat) int { return a.CreatedAt.Compare(b.CreatedAt) })
	labels := make(map[string]string, len(chats))
	counts := make(map[Role]int)
	for _, chat := range ordered {
		label := chat.Title
		if label == "" {
			label = "New chat"
		}
		switch chat.Role {
		case RoleOperator:
			// Operator titles stay exactly as stored.
		case RoleBuild, RoleVerify, RoleRepair:
			counts[chat.Role]++
			if systemTitle(chat, sessionTitle) {
				label = string(chat.Role)
				if counts[chat.Role] > 1 {
					label = fmt.Sprintf("%s %d", label, counts[chat.Role])
				}
				title := resumedSuffix.ReplaceAllString(strings.TrimSpace(chat.Title), "")
				if chat.Role == RoleRepair {
					if match := repairReason.FindStringSubmatch(title); match != nil && match[1] != "" {
						label += ": " + match[1]
					}
				}
			}
		}
		labels[chat.ID] = label
	}
	return labels
}

func systemTitle(chat Chat, sessionTitle string) bool {
	title := resumedSuffix.ReplaceAllString(strings.TrimSpace(chat.Title), "")
	if title == "" || strings.EqualFold(title, "new chat") || title == sessionTitle {
		return true
	}
	if match := numberedRole.FindStringSubmatch(title); match != nil && strings.EqualFold(match[1], string(chat.Role)) {
		return true
	}
	return repairReason.MatchString(title) || strings.HasPrefix(title, `Repair: `) || (strings.HasPrefix(title, `Run "`) && strings.HasSuffix(title, `"`))
}
