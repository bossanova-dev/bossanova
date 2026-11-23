package models

import (
	"fmt"
	"strings"
	"time"
)

// SessionLabelMaxNameLength caps a session label's name in bytes after trimming.
const SessionLabelMaxNameLength = 64

// SessionLabelMaxDescriptionLength caps a session label's optional description.
const SessionLabelMaxDescriptionLength = 256

// DefaultLabelColor is the fallback hex color when none is supplied (Linear gray).
const DefaultLabelColor = "#95a2b3"

// LabelColorPreset is one named color in the Linear-style label palette.
type LabelColorPreset struct {
	Name string
	Hex  string
}

// LabelColorPresets lists the Linear-style preset colors for session labels.
var LabelColorPresets = []LabelColorPreset{
	{Name: "red", Hex: "#eb5757"},
	{Name: "orange", Hex: "#f2994a"},
	{Name: "yellow", Hex: "#f2c94c"},
	{Name: "green", Hex: "#4cb782"},
	{Name: "teal", Hex: "#26b5ce"},
	{Name: "blue", Hex: "#4ea7fc"},
	{Name: "indigo", Hex: "#5e6ad2"},
	{Name: "purple", Hex: "#bb87fc"},
	{Name: "pink", Hex: "#e272b8"},
	{Name: "gray", Hex: "#95a2b3"},
}

// SessionLabel is a colored tag that can be attached to sessions.
type SessionLabel struct {
	ID           string    `json:"id"`
	Name         string    `json:"name"`
	Color        string    `json:"color"`
	Description  string    `json:"description,omitempty"`
	CreatedAt    time.Time `json:"created_at"`
	UpdatedAt    time.Time `json:"updated_at"`
	SessionCount int       `json:"session_count,omitempty"`
}

// NormalizeLabelName trims and validates a label name.
func NormalizeLabelName(raw string) (string, error) {
	name := strings.TrimSpace(raw)
	if name == "" {
		return "", fmt.Errorf("label name is required")
	}
	if len(name) > SessionLabelMaxNameLength {
		return "", fmt.Errorf("label name exceeds %d characters", SessionLabelMaxNameLength)
	}
	return name, nil
}

// NormalizeLabelDescription trims and validates a label description.
func NormalizeLabelDescription(raw string) (string, error) {
	desc := strings.TrimSpace(raw)
	if len(desc) > SessionLabelMaxDescriptionLength {
		return "", fmt.Errorf("label description exceeds %d characters", SessionLabelMaxDescriptionLength)
	}
	return desc, nil
}

// NormalizeLabelColor resolves a preset name (e.g. "red") or #rrggbb hex string
// into a lowercase #rrggbb hex color. An empty string resolves to DefaultLabelColor.
func NormalizeLabelColor(raw string) (string, error) {
	c := strings.ToLower(strings.TrimSpace(raw))
	if c == "" {
		return DefaultLabelColor, nil
	}
	for _, preset := range LabelColorPresets {
		if c == preset.Name {
			return preset.Hex, nil
		}
	}
	if !strings.HasPrefix(c, "#") {
		c = "#" + c
	}
	if len(c) != 7 {
		return "", fmt.Errorf("invalid label color %q: expected #rrggbb or preset name", raw)
	}
	for i := 1; i < 7; i++ {
		ch := c[i]
		if (ch < '0' || ch > '9') && (ch < 'a' || ch > 'f') {
			return "", fmt.Errorf("invalid label color %q: expected #rrggbb or preset name", raw)
		}
	}
	return c, nil
}
