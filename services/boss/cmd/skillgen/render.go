package skillgen

import (
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/spf13/pflag"

	"github.com/recurser/bossalib/clidoc"
)

// routingDirective sits above the index table. The index only routes; it must
// never be mistaken for the reference itself.
const routingDirective = "Every `boss` command is documented in one of the reference files below, grouped as `boss --help`\n" +
	"groups them. **Open the matching reference before using a command** — never infer a command's\n" +
	"syntax, arguments or flags from an index row."

// renderGlobalFlags returns the "## Global Flags" section for the root
// command's persistent flags. Its bytes are unchanged by the per-group split:
// the section stays resident in SKILL.md.
func renderGlobalFlags(globalFlags *pflag.FlagSet) string {
	var b strings.Builder
	b.WriteString("## Global Flags\n\n")
	var gfs []*pflag.Flag
	globalFlags.VisitAll(func(f *pflag.Flag) {
		if !f.Hidden {
			gfs = append(gfs, f)
		}
	})
	sort.Slice(gfs, func(i, j int) bool { return gfs[i].Name < gfs[j].Name })
	for _, f := range gfs {
		b.WriteString("### `--")
		b.WriteString(f.Name)
		b.WriteString("`\n\n")
		b.WriteString(f.Usage)
		b.WriteString("\n\n")
	}
	return b.String()
}

// renderGroup returns one group's section: the "## <Title>" heading followed by
// every command, ending in exactly one newline. This is the body of a
// references/<id>.md file, byte-identical to the section the single-blob
// renderer used to emit inline.
func renderGroup(g clidoc.GroupDoc) string {
	var b strings.Builder
	b.WriteString("## ")
	b.WriteString(g.Title)
	b.WriteString("\n\n")
	for _, c := range g.Commands {
		renderCommand(&b, c)
	}
	return strings.TrimRight(b.String(), "\n") + "\n"
}

// renderIndex returns the resident SKILL.md body: the global flags section, a
// routing directive, and a table pointing at one references/<id>.md per group.
//
// The table is emitted already column-padded because prettier's markdown
// printer pads every cell to its column's widest cell. Emitting it unpadded
// would make `make format` reflow the generated region and put the drift gate
// into a permanent ping-pong.
func renderIndex(groups []clidoc.GroupDoc, globalFlags *pflag.FlagSet) string {
	var b strings.Builder
	b.WriteString(renderGlobalFlags(globalFlags))
	b.WriteString("## Command Groups\n\n")
	b.WriteString(routingDirective)
	b.WriteString("\n\n")

	rows := [][2]string{{"Reference", "Read it when…"}}
	for _, g := range groups {
		rows = append(rows, [2]string{"`references/" + g.ID + ".md`", g.ReadWhen})
	}
	w := [2]int{3, 3} // GFM requires at least three dashes in the separator row
	for _, r := range rows {
		for i := range w {
			if n := utf8.RuneCountInString(r[i]); n > w[i] {
				w[i] = n
			}
		}
	}
	writeRow := func(r [2]string) {
		b.WriteString("| ")
		b.WriteString(padRunes(r[0], w[0]))
		b.WriteString(" | ")
		b.WriteString(padRunes(r[1], w[1]))
		b.WriteString(" |\n")
	}
	writeRow(rows[0])
	b.WriteString("| " + strings.Repeat("-", w[0]) + " | " + strings.Repeat("-", w[1]) + " |\n")
	for _, r := range rows[1:] {
		writeRow(r)
	}

	return strings.TrimRight(b.String(), "\n") + "\n"
}

// padRunes right-pads s with spaces to width runes. Prettier measures markdown
// table cells in display width, and every character this table can contain
// (including "…" and "—") is one column wide, so rune count is the right ruler.
func padRunes(s string, width int) string {
	if n := utf8.RuneCountInString(s); n < width {
		return s + strings.Repeat(" ", width-n)
	}
	return s
}

func renderCommand(b *strings.Builder, c clidoc.CommandDoc) {
	b.WriteString("### `")
	b.WriteString(c.UsageLine)
	b.WriteString("`\n\n")
	// A cobra alias replaces the command's own leaf name, so render it under the
	// parent path (e.g. alias "ls" of "boss plugin list" is "boss plugin ls"),
	// not bare under "boss".
	aliasPrefix := commandParent(c.Path)
	for _, alias := range c.Aliases {
		b.WriteString("Alias: `")
		b.WriteString(aliasPrefix)
		b.WriteString(" ")
		b.WriteString(alias)
		b.WriteString("`\n\n")
	}
	if c.Synopsis != "" {
		b.WriteString(c.Synopsis)
		b.WriteString("\n\n")
	}
	if c.Long != "" {
		b.WriteString(c.Long)
		b.WriteString("\n\n")
	}
	if len(c.Flags) > 0 {
		b.WriteString("**Flags:**\n\n")
		for _, f := range c.Flags {
			renderFlag(b, f)
		}
		b.WriteString("\n")
	}
	if len(c.Examples) > 0 {
		b.WriteString("```bash\n")
		for _, ex := range c.Examples {
			if ex.Explanation != "" {
				b.WriteString("# ")
				b.WriteString(ex.Explanation)
				b.WriteString("\n")
			}
			b.WriteString(ex.Command)
			b.WriteString("\n")
		}
		b.WriteString("```\n\n")
	}
}

// commandParent returns a command path with its leaf name removed, e.g.
// "boss plugin list" -> "boss plugin". A path with no space (just "boss") is
// returned unchanged.
func commandParent(path string) string {
	if i := strings.LastIndex(path, " "); i >= 0 {
		return path[:i]
	}
	return path
}

func renderFlag(b *strings.Builder, f clidoc.FlagDoc) {
	b.WriteString("- `--")
	b.WriteString(f.Name)
	b.WriteString("`")
	if f.Shorthand != "" {
		b.WriteString(", `-")
		b.WriteString(f.Shorthand)
		b.WriteString("`")
	}
	b.WriteString(" — ")
	b.WriteString(f.Usage)
	if f.Default != "" {
		b.WriteString(" (default: ")
		b.WriteString(f.Default)
		b.WriteString(")")
	}
	if f.Required {
		b.WriteString(" (required)")
	}
	if f.Deprecated {
		b.WriteString(" (deprecated: ")
		b.WriteString(f.DeprecationMsg)
		b.WriteString(")")
	}
	b.WriteString("\n")
}
