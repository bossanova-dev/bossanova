package skillgen

import (
	"sort"
	"strings"

	"github.com/spf13/pflag"

	"github.com/recurser/bossalib/clidoc"
)

// Render returns the generated reference body (between, but not including, the
// markers). It owns all formatting: no terminal wrapping, stable ordering.
func Render(groups []clidoc.GroupDoc, globalFlags *pflag.FlagSet) string {
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

	for _, g := range groups {
		b.WriteString("## ")
		b.WriteString(g.Title)
		b.WriteString("\n\n")
		for _, c := range g.Commands {
			renderCommand(&b, c)
		}
	}

	return strings.TrimRight(b.String(), "\n") + "\n"
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
