package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
)

// These role names and defaults mirror the embedded skill-config module.
var linearStateDefaults = map[string]string{"unplanned": "Backlog", "planned": "Todo", "inProgress": "In Progress", "inReview": "In Review", "done": "Done"}
var linearLabelDefaults = map[string]string{"agentBuild": "agent-build", "needsHuman": "needs-human", "agentPlan": "agent-plan", "agentQuestion": "agent-question", "epic": "epic"}

type linearOverrides struct {
	Team, TeamKey  string
	States, Labels map[string]string
	AssigneeMe     bool
}

func parseLinearOverrides(states, labels []string, me bool) (linearOverrides, error) {
	ov := linearOverrides{States: map[string]string{}, Labels: map[string]string{}, AssigneeMe: me}
	for _, group := range []struct {
		values []string
		dest   map[string]string
	}{{states, ov.States}, {labels, ov.Labels}} {
		for _, value := range group.values {
			role, name, ok := strings.Cut(value, "=")
			if !ok {
				return ov, fmt.Errorf("override %q must be role=name", value)
			}
			group.dest[role] = name
			if err := ov.validate(); err != nil {
				return ov, err
			}
		}
	}
	return ov, ov.validate()
}

func (ov linearOverrides) validate() error {
	for _, group := range []struct {
		kind             string
		values, defaults map[string]string
	}{{"state", ov.States, linearStateDefaults}, {"label", ov.Labels, linearLabelDefaults}} {
		for _, role := range sortedOverrideKeys(group.values) {
			if _, ok := group.defaults[role]; !ok || group.values[role] == "" {
				return fmt.Errorf("invalid --%s %s=%s; name must be non-empty; valid roles: %s", group.kind, role, group.values[role], strings.Join(sortedOverrideKeys(group.defaults), ", "))
			}
		}
	}
	return nil
}

// seedOverridesFromExisting fills each state and label role the flags left
// unset with the name an existing config already maps it to, so a --merge
// re-run maps the operator's earlier choice instead of the default. Flags win.
// An undecodable config seeds nothing; the merge itself reports it later.
func seedOverridesFromExisting(existing []byte, flags linearOverrides) linearOverrides {
	var doc struct {
		TrackerConfig struct {
			Linear struct {
				States map[string]any `json:"states"`
				Labels map[string]any `json:"labels"`
			} `json:"linear"`
		} `json:"trackerConfig"`
	}
	if json.Unmarshal(existing, &doc) != nil {
		return flags
	}
	ov := flags
	ov.States, ov.Labels = map[string]string{}, map[string]string{}
	for _, group := range []struct {
		from     map[string]any
		flagged  map[string]string
		dest     map[string]string
		defaults map[string]string
	}{
		{doc.TrackerConfig.Linear.States, flags.States, ov.States, linearStateDefaults},
		{doc.TrackerConfig.Linear.Labels, flags.Labels, ov.Labels, linearLabelDefaults},
	} {
		for role, value := range group.from {
			name, ok := value.(string)
			if _, known := group.defaults[role]; known && ok && name != "" {
				group.dest[role] = name
			}
		}
		for role, name := range group.flagged {
			group.dest[role] = name
		}
	}
	return ov
}

func (ov linearOverrides) hasRoleOverrides() bool {
	return len(ov.States) > 0 || len(ov.Labels) > 0 || ov.AssigneeMe
}
func sortedOverrideKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

type jsonMember struct {
	key   string
	value json.RawMessage
}
type orderedObject []jsonMember

// Decode values as raw bytes: objects we never touch retain their ordering and
// numbers never pass through floating point conversion.
func readOrderedObject(data []byte) (orderedObject, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	token, err := dec.Token()
	if err != nil {
		return nil, err
	}
	if token != json.Delim('{') {
		return nil, fmt.Errorf("expected JSON object")
	}
	object := orderedObject{}
	for dec.More() {
		token, err = dec.Token()
		if err != nil {
			return nil, err
		}
		key, ok := token.(string)
		if !ok {
			return nil, fmt.Errorf("expected object key")
		}
		if _, exists := object.get(key); exists {
			return nil, fmt.Errorf("duplicate JSON key %q", key)
		}
		var raw json.RawMessage
		if err = dec.Decode(&raw); err != nil {
			return nil, err
		}
		object = append(object, jsonMember{key, raw})
	}
	if _, err = dec.Token(); err != nil {
		return nil, err
	}
	if _, err = dec.Token(); err != io.EOF {
		return nil, fmt.Errorf("trailing JSON data")
	}
	return object, nil
}
func (o orderedObject) get(key string) (json.RawMessage, bool) {
	for _, m := range o {
		if m.key == key {
			return m.value, true
		}
	}
	return nil, false
}
func (o *orderedObject) put(key string, value json.RawMessage) {
	for i := range *o {
		if (*o)[i].key == key {
			(*o)[i].value = value
			return
		}
	}
	*o = append(*o, jsonMember{key, value})
}

//nolint:unparam // The json.Marshaler interface requires an error result.
func (o orderedObject) MarshalJSON() ([]byte, error) {
	var b bytes.Buffer
	b.WriteByte('{')
	for i, m := range o {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(quoteKey(m.key))
		b.WriteByte(':')
		b.Write(m.value)
	}
	b.WriteByte('}')
	return b.Bytes(), nil
}
func rawJSON(v any) (json.RawMessage, error) {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimSpace(b.Bytes()), nil
}

type mergeResult struct {
	bytes                               []byte
	unchanged                           bool
	added, changed, preserved, warnings []string
	formattingNormalized                bool
}

// mergeInitConfig is the persistence seam for init's interactive and mapping
// callers. It validates the full candidate before atomically replacing a file.
func mergeInitConfig(target string, existing []byte, detected detectedConfig, ov linearOverrides) (mergeResult, error) {
	return mergeInitConfigMode(target, existing, detected, ov, false, true)
}

// replace authorizes replacing an entry; force additionally permits symlinks.
func mergeInitConfigMode(target string, existing []byte, detected detectedConfig, ov linearOverrides, force, replace bool) (mergeResult, error) {
	r := mergeResult{}
	if err := ov.validate(); err != nil {
		return r, err
	}
	if info, err := os.Lstat(target); err == nil && info.Mode()&os.ModeSymlink != 0 && !force {
		return r, fmt.Errorf("refusing to merge symlink %s", target)
	} else if err != nil && !os.IsNotExist(err) {
		return r, err
	}
	source := existing
	if source == nil {
		source = []byte(`{}`)
	}
	root, err := readOrderedObject(source)
	if err != nil {
		return r, fmt.Errorf("read config: %w", err)
	}
	// Set only the selected leaf, appending new members at every level.
	var setPath func(*orderedObject, []string, any, bool, string) error
	setPath = func(object *orderedObject, keys []string, value any, onlyAbsent bool, prefix string) error {
		old, exists := object.get(keys[0])
		name := prefix + keys[0]
		if len(keys) == 1 {
			raw, err := rawJSON(value)
			if err != nil {
				return err
			}
			if exists && (onlyAbsent || jsonEqual(old, raw)) {
				r.preserved = append(r.preserved, name)
				return nil
			}
			object.put(keys[0], raw)
			if exists {
				r.changed = append(r.changed, name)
			} else {
				r.added = append(r.added, name)
			}
			return nil
		}
		child := orderedObject{}
		if exists {
			var err error
			child, err = readOrderedObject(old)
			if err != nil {
				return fmt.Errorf("%s must be an object: %w", keys[0], err)
			}
		}
		before := len(r.added) + len(r.changed)
		if err := setPath(&child, keys[1:], value, onlyAbsent, name+"."); err != nil {
			return err
		}
		if len(r.added)+len(r.changed) > before {
			raw, err := rawJSON(child)
			if err != nil {
				return err
			}
			object.put(keys[0], raw)
		}
		return nil
	}
	set := func(object *orderedObject, keys []string, value any, onlyAbsent bool) error {
		return setPath(object, keys, value, onlyAbsent, "")
	}
	for _, k := range sortedOverrideKeys(detected.Commands) {
		if err := set(&root, []string{"commands", k}, detected.Commands[k], true); err != nil {
			return r, err
		}
	}
	base := []string{"trackerConfig", "linear"}
	for _, item := range []struct{ key, value string }{{"team", ov.Team}, {"teamKey", ov.TeamKey}} {
		if item.value != "" {
			if err := set(&root, append(base, item.key), item.value, false); err != nil {
				return r, err
			}
		}
	}
	for _, group := range []struct {
		key              string
		values, defaults map[string]string
	}{{"states", ov.States, linearStateDefaults}, {"labels", ov.Labels, linearLabelDefaults}} {
		for _, role := range sortedOverrideKeys(group.values) {
			// Choosing a default never overwrites an explicit user-owned role.
			if group.values[role] == group.defaults[role] {
				continue
			}
			if err := set(&root, []string{"trackerConfig", "linear", group.key, role}, group.values[role], false); err != nil {
				return r, err
			}
		}
	}
	if ov.AssigneeMe {
		selection, err := objectAt(root, []string{"trackerConfig", "linear", "selection"})
		if err != nil {
			return r, err
		}
		if _, exists := selection.get("assignees"); exists {
			r.warnings = append(r.warnings, "existing trackerConfig.linear.selection.assignees slot preserved")
		} else {
			stages, err := objectAt(selection, []string{"stages"})
			if err != nil {
				return r, err
			}
			for _, stage := range stages {
				obj, err := readOrderedObject(stage.value)
				if err != nil {
					return r, err
				}
				if _, exists := obj.get("assignees"); exists {
					r.warnings = append(r.warnings, "selection.stages."+stage.key+".assignees overrides the shared assignee filter")
				}
			}
			if err := set(&root, []string{"trackerConfig", "linear", "selection", "assignees", "include"}, []string{"me"}, false); err != nil {
				return r, err
			}
		}
	}
	raw, err := rawJSON(root)
	if err != nil {
		return r, err
	}
	r.unchanged = existing != nil && jsonEqual(existing, raw)
	if r.unchanged {
		r.bytes = existing
	} else {
		var b bytes.Buffer
		enc := json.NewEncoder(&b)
		enc.SetEscapeHTML(false)
		enc.SetIndent("", "  ")
		if err := enc.Encode(root); err != nil {
			return r, err
		}
		r.bytes = b.Bytes()
		r.formattingNormalized = existing != nil
	}
	for _, m := range root {
		touched := false
		for _, key := range append(append([]string{}, r.added...), r.changed...) {
			if strings.HasPrefix(key, m.key+".") || key == m.key {
				touched = true
			}
		}
		if !touched {
			r.preserved = append(r.preserved, m.key)
		}
	}
	bridge, cleanup, err := newNodeBridge()
	if err != nil {
		return r, err
	}
	defer cleanup()
	if err := bridge.validate(r.bytes, target); err != nil {
		return r, err
	}
	if !r.unchanged {
		if err := writeMergedConfig(target, r.bytes, force, replace); err != nil {
			return r, err
		}
	}
	return r, nil
}
func objectAt(root orderedObject, keys []string) (orderedObject, error) {
	for _, key := range keys {
		raw, exists := root.get(key)
		if !exists {
			return orderedObject{}, nil
		}
		var err error
		root, err = readOrderedObject(raw)
		if err != nil {
			return nil, fmt.Errorf("%s must be an object: %w", key, err)
		}
	}
	return root, nil
}
func jsonEqual(a, b []byte) bool {
	decode := func(data []byte) any {
		dec := json.NewDecoder(bytes.NewReader(data))
		dec.UseNumber()
		var value any
		if err := dec.Decode(&value); err != nil {
			return nil
		}
		return value
	}
	return reflect.DeepEqual(decode(a), decode(b))
}
func writeMergedConfig(target string, data []byte, force, replace bool) error {
	mode := os.FileMode(0o644)
	if info, err := os.Lstat(target); err == nil {
		if info.Mode()&os.ModeSymlink != 0 && !force {
			return fmt.Errorf("refusing to merge symlink %s", target)
		}
		if info.Mode().IsRegular() {
			mode = info.Mode().Perm()
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(target), ".boss-skills-*")
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(f.Name()) }()
	if err := f.Chmod(mode); err != nil {
		_ = f.Close()
		return err
	}
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if !replace {
		// Link publishes the completed same-directory file atomically, refusing
		// any entry created since the command's initial existence check.
		if err := os.Link(f.Name(), target); err != nil {
			if os.IsExist(err) {
				return fmt.Errorf("%s already exists; re-run with --merge or --force", target)
			}
			return fmt.Errorf("create %s: %w", target, err)
		}
		return nil
	}
	// Rename replaces the entry rather than following a link that appeared later.
	if info, err := os.Lstat(target); err == nil && info.Mode()&os.ModeSymlink != 0 && !force {
		return fmt.Errorf("refusing to merge symlink %s", target)
	}
	return os.Rename(f.Name(), target)
}
func (r mergeResult) reportText(target string) string {
	var b strings.Builder
	b.WriteString("Merged " + target + "\n")
	for _, group := range []struct {
		name string
		keys []string
	}{{"added", r.added}, {"changed", r.changed}, {"preserved", r.preserved}} {
		fmt.Fprintf(&b, "  %s: %s\n", group.name, strings.Join(group.keys, ", "))
	}
	for _, warning := range r.warnings {
		b.WriteString("Warning: " + warning + "\n")
	}
	if r.formattingNormalized {
		b.WriteString("  formatting normalized\n")
	}
	if r.unchanged {
		b.WriteString("  unchanged; write skipped\n")
	}
	return b.String()
}
