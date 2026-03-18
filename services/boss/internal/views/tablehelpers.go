package views

import (
	"charm.land/bubbles/v2/key"
	"charm.land/bubbles/v2/table"
)

// bossKeyMap returns the default table KeyMap with "d" removed from
// HalfPageDown (conflicts with delete in most views). Only "ctrl+d" remains.
func bossKeyMap() table.KeyMap {
	km := table.DefaultKeyMap()
	km.HalfPageDown = key.NewBinding(
		key.WithKeys("ctrl+d"),
		key.WithHelp("ctrl+d", "½ page down"),
	)
	return km
}
