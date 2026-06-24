// Package skillgen extracts the boss CLI command tree into clidoc's neutral
// doc model and renders it to the /boss skill's generated reference block.
package skillgen

import (
	"fmt"
	"sort"
	"strings"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"

	"github.com/recurser/bossalib/clidoc"
)

const requiredFlagAnnotation = cobra.BashCompOneRequiredFlag

// Extract walks root and returns the doc model grouped per clidoc.GroupOrder.
// It returns an error naming every command whose group id is empty or unknown.
func Extract(root *cobra.Command) ([]clidoc.GroupDoc, error) {
	byGroup := map[string][]clidoc.CommandDoc{}
	var ungrouped []string

	var walk func(cmd *cobra.Command, path, inheritedGroup string)
	walk = func(cmd *cobra.Command, path, inheritedGroup string) {
		group := cmd.GroupID
		if group == "" {
			group = inheritedGroup
		}
		skip := cmd.Hidden || cmd.Name() == "help" || cmd.Name() == "completion" || cmd.Deprecated != ""
		isGroup := cmd.Run == nil && cmd.RunE == nil && cmd.HasSubCommands()

		if !skip && cmd != root {
			if group == "" {
				ungrouped = append(ungrouped, path)
			} else if _, ok := clidoc.GroupTitle(group); !ok {
				ungrouped = append(ungrouped, fmt.Sprintf("%s (unknown group %q)", path, group))
			} else {
				byGroup[group] = append(byGroup[group], commandDoc(cmd, path, group, isGroup))
			}
		}
		for _, child := range cmd.Commands() {
			walk(child, path+" "+child.Name(), group)
		}
	}
	walk(root, "boss", "")

	if len(ungrouped) > 0 {
		sort.Strings(ungrouped)
		return nil, fmt.Errorf("commands have no/unknown GroupID (assign one via AddGroup+GroupID): %s",
			strings.Join(ungrouped, ", "))
	}

	var out []clidoc.GroupDoc
	for _, spec := range clidoc.GroupOrder {
		cmds := byGroup[spec.ID]
		if len(cmds) == 0 {
			continue
		}
		sort.Slice(cmds, func(i, j int) bool { return cmds[i].Path < cmds[j].Path })
		out = append(out, clidoc.GroupDoc{ID: spec.ID, Title: spec.Title, Commands: cmds})
	}
	return out, nil
}

func commandDoc(cmd *cobra.Command, path, group string, isGroup bool) clidoc.CommandDoc {
	prose := clidoc.Registry[path]
	doc := clidoc.CommandDoc{
		Path:           path,
		Synopsis:       cmd.Short,
		UsageLine:      usageLine(cmd, path),
		Long:           prose.Long,
		Aliases:        append([]string(nil), cmd.Aliases...),
		Examples:       prose.Examples,
		IsGroup:        isGroup,
		GroupID:        group,
		DeprecationMsg: cmd.Deprecated,
	}
	cmd.Flags().VisitAll(func(f *pflag.Flag) {
		if f.Hidden {
			return
		}
		doc.Flags = append(doc.Flags, flagDoc(f))
	})
	sort.Slice(doc.Flags, func(i, j int) bool { return doc.Flags[i].Name < doc.Flags[j].Name })
	return doc
}

func usageLine(cmd *cobra.Command, path string) string {
	use := strings.TrimSpace(cmd.Use)
	tail := ""
	if i := strings.IndexAny(use, " \t"); i >= 0 {
		tail = strings.TrimSpace(use[i:])
	}
	if tail != "" {
		return path + " " + tail + flagsSuffix(cmd)
	}
	return path + flagsSuffix(cmd)
}

func flagsSuffix(cmd *cobra.Command) string {
	if cmd.HasAvailableFlags() {
		return " [flags]"
	}
	return ""
}

func flagDoc(f *pflag.Flag) clidoc.FlagDoc {
	d := clidoc.FlagDoc{
		Name:           f.Name,
		Shorthand:      f.Shorthand,
		Usage:          f.Usage,
		Deprecated:     f.Deprecated != "",
		DeprecationMsg: f.Deprecated,
	}
	// Omit zero-value defaults that carry no information: the empty string, the
	// "false" of an unset bool, and the "[]" of an unset slice flag.
	if f.DefValue != "" && f.DefValue != "false" && f.DefValue != "[]" {
		d.Default = f.DefValue
	}
	if _, ok := f.Annotations[requiredFlagAnnotation]; ok {
		d.Required = true
	}
	return d
}
