// Package skillinstall provides a generic agent-skills installer.
// It reads skill files from a caller-supplied embed.FS and writes them to a
// target directory, managing the bossanova namespace and creating symlinks so
// that Claude discovers the skills as top-level entries.
//
// The default install directory is ~/.claude/skills, which is the Claude Code
// skills directory. This is intentionally opinionated: bossanova skills are
// always installed there.
package skillinstall
