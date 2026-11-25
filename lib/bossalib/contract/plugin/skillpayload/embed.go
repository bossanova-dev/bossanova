// Package skillpayload embeds the published boss-* skill tree. It is the one
// payload the boss CLI, bossd's factoryskills adapter and bossd-plugin-claude
// ship, so the binaries carry identical bytes without a committed mirror. It is
// kept separate from lib/bossalib/contract/plugin/skillinstall on purpose:
// importers that only read records (revisiondrift, factoryversions) do not link
// the payload, while bossd's factoryskills adapter links it deliberately to
// install on consent and refresh after upgrades.
package skillpayload

import "embed"

// SkillsFS contains the embedded boss skill files, rooted at skills/.
// Edit the payload under lib/bossalib/contract/plugin/skillpayload/skills; this directory is the
// checkout source skillinstall.SourceRelPath points at.
//
//go:embed all:skills
var SkillsFS embed.FS
