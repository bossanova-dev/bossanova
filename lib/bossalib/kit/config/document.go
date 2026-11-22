package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// ErrDocumentChanged reports that settings.json changed between LoadDocument
// and Save. The caller re-reads and re-applies its change.
var ErrDocumentChanged = errors.New("settings.json changed while it was being edited")

// Document is settings.json decoded as plain JSON rather than into Settings.
// A writer that owns a few keys edits only those, and every other key keeps
// the value the file held, including keys this build does not know. Save
// writes keys in sorted order.
type Document struct {
	path    string
	existed bool
	read    []byte
	root    map[string]any
}

// LoadDocument reads the settings file at path. A missing file is an empty
// document.
func LoadDocument(path string) (*Document, error) {
	cleaned := filepath.Clean(path)
	data, err := os.ReadFile(cleaned)
	if err != nil && !os.IsNotExist(err) {
		return nil, err
	}
	doc := &Document{path: cleaned, existed: err == nil, read: data, root: map[string]any{}}
	if doc.existed {
		dec := json.NewDecoder(bytes.NewReader(data))
		dec.UseNumber()
		if err := dec.Decode(&doc.root); err != nil {
			return nil, fmt.Errorf("parse settings %q: %w", cleaned, err)
		}
		if doc.root == nil {
			doc.root = map[string]any{}
		}
	}
	return doc, nil
}

// Settings decodes the document as LoadFrom decodes the file. Each call
// returns an independent value.
func (d *Document) Settings() (Settings, error) {
	data, err := json.Marshal(d.root)
	if err != nil {
		return DefaultSettings(), fmt.Errorf("marshal settings %q: %w", d.path, err)
	}
	return decodeSettings(data, d.path)
}

// Set writes value at the key path, creating objects on the way. A nil value
// removes the key, as Save's omitempty fields do.
func (d *Document) Set(value any, path ...string) {
	if path[0] == "managed_accounts" {
		d.adoptLegacyRotation()
	}
	obj := d.root
	for _, key := range path[:len(path)-1] {
		next, ok := obj[key].(map[string]any)
		if !ok {
			if value == nil {
				return
			}
			next = map[string]any{}
			obj[key] = next
		}
		obj = next
	}
	leaf := path[len(path)-1]
	if value == nil {
		delete(obj, leaf)
		return
	}
	obj[leaf] = value
}

// adoptLegacyRotation renames a lone legacy "rotation" block to
// managed_accounts. Settings prefers managed_accounts whenever it is present,
// so writing one key into a new managed_accounts block would drop the
// rotation block's other keys.
func (d *Document) adoptLegacyRotation() {
	legacy, ok := d.root["rotation"]
	if _, managed := d.root["managed_accounts"]; managed || !ok {
		return
	}
	d.root["managed_accounts"] = legacy
	delete(d.root, "rotation")
}

// SetPluginConfig writes plugins[name].config[key], as SetPluginConfigString
// does: an empty value removes the key, and a plugin without an entry gets
// one.
func (d *Document) SetPluginConfig(name, key, value string) {
	plugins, _ := d.root["plugins"].([]any)
	for _, raw := range plugins {
		plugin, ok := raw.(map[string]any)
		if !ok || plugin["name"] != name {
			continue
		}
		cfg, _ := plugin["config"].(map[string]any)
		if value == "" {
			delete(cfg, key)
			if len(cfg) == 0 {
				delete(plugin, "config")
			}
			return
		}
		if cfg == nil {
			cfg = map[string]any{}
			plugin["config"] = cfg
		}
		cfg[key] = value
		return
	}
	if value == "" {
		return
	}
	d.root["plugins"] = append(plugins, map[string]any{
		"name": name, "path": "", "enabled": false, "config": map[string]any{key: value},
	})
}

// Save replaces the file atomically. It returns ErrDocumentChanged, and
// leaves the file as it is, when the file no longer holds what LoadDocument
// read. A writer that does not take part, such as the TUI, can still land in
// the instant between that check and the rename.
func (d *Document) Save() error {
	data, err := json.MarshalIndent(d.root, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal settings %q: %w", d.path, err)
	}
	return replaceSettingsFile(d.path, append(data, '\n'), syncDir, d.unchanged)
}

func (d *Document) unchanged() error {
	now, err := os.ReadFile(d.path)
	switch {
	case os.IsNotExist(err) && !d.existed:
		return nil
	case os.IsNotExist(err):
		return ErrDocumentChanged
	case err != nil:
		return fmt.Errorf("re-read settings %q: %w", d.path, err)
	case !d.existed || !bytes.Equal(now, d.read):
		return ErrDocumentChanged
	}
	return nil
}
