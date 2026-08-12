// Package skillinstall embeds boss skill files for extraction at CLI startup.
package skillinstall

import "embed"

// SkillsFS contains the embedded boss skill files.
// The skills/ directory here is the canonical source: `make copy-skills` rsyncs
// it OUT to plugins/bossd-plugin-claude/skilldata/skills so both binaries embed
// identical bytes. Edit the payload here, never in the mirror.
//
//go:embed all:skills
var SkillsFS embed.FS
