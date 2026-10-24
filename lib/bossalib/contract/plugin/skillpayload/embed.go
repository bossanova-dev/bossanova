// Package skillpayload embeds the published boss-* skill tree. It is the one
// payload both the boss CLI (extracted at startup) and bossd-plugin-claude
// (restored at daemon start) ship, so the two binaries carry identical bytes
// without a committed mirror. It is kept separate from lib/bossalib/contract/plugin/skillinstall
// on purpose: bossd and revisiondrift import the installer and must not link a
// multi-megabyte payload they never read.
package skillpayload

import "embed"

// SkillsFS contains the embedded boss skill files, rooted at skills/.
// Edit the payload under lib/bossalib/contract/plugin/skillpayload/skills; this directory is the
// checkout source skillinstall.SourceRelPath points at.
//
//go:embed all:skills
var SkillsFS embed.FS
