// Package skilldata embeds boss skill files for extraction at daemon startup.
package skilldata

import "embed"

// SkillsFS contains the embedded boss skill files.
// The skills/ directory is populated by `make copy-skills` before build.
//
//go:embed all:skills
var SkillsFS embed.FS
