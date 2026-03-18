# TUI Design Guidelines

Reference guide for building consistent Bubbletea v2 views in the boss CLI. Consult this skill when creating or modifying any TUI view.

---

## Layout Constants

Shared layout constants live in `services/boss/internal/views/`:

```go
// home.go
const (
    shortIDLen    = 7 // characters shown for truncated UUIDs
    actionBarPadY = 1 // blank lines above action bar
)

// tablehelpers.go
const defaultTableHeight = 20 // fallback before first WindowSizeMsg
```

**Always use these constants** — never hardcode `7` or `1` for these purposes.

---

## Shared Styles

Defined in `services/boss/internal/views/home.go` and `tablehelpers.go`:

| Variable         | Purpose                               |
| ---------------- | ------------------------------------- |
| `styleTitle`     | Bold heading with horizontal pad      |
| `styleSelected`  | Bold text for cursor-highlighted      |
| `styleActionBar` | Faint bottom menu with top pad        |
| `styleError`     | Red text with padding                 |
| `styleSubtle`    | Faint secondary text                  |
| `colorGreen`     | Success / merged / green states       |
| `colorYellow`    | In-progress / pending states          |
| `colorRed`       | Error / blocked states                |
| `colorCyan`      | Transitional states                   |
| `colorGray`      | Unknown / default states              |
| `colorSelected`  | `#A8B1F4` — selected row fg + chevron |

---

## View Structure

Every view follows this top-to-bottom structure:

```
[heading]          styleTitle.Render("Title") + "\n\n"
[blank line]
[table]            bubbles table.Model with ❯ cursor column
[action bar]       styleActionBar.Render("[key] action  ...")
```

### Rules

1. **One blank line** between heading and content — the `View()` method writes the title + `"\n"`, so step view functions must start with `b.WriteString("\n")` to produce the blank line
2. **No explicit newline before `styleActionBar`** — it has `Padding(actionBarPadY, 2)` built in, which provides the blank line above. This applies everywhere, including confirmation dialogs.
3. **Table views** use `table.Model` from `charm.land/bubbles/v2/table` — see Table Formatting below.
4. **Non-table lists** use manual `"❯ "` / `"  "` cursor prefix — see Select Lists below.

### Action Bar Spacing

`styleActionBar` has `Padding(actionBarPadY, 2)` which adds one blank line above the text. For this to work correctly:

1. The preceding content **must** end with `"\n"` so the action bar starts on its own line
2. Do **not** add extra `"\n"` beyond that — the padding handles the blank line

```go
// WRONG — no newline before, action bar merges with previous line
b.WriteString(lipgloss.NewStyle().Padding(0, 2).Render("Some text"))
b.WriteString(styleActionBar.Render("[q] quit"))

// WRONG — double blank line (extra \n + padding)
b.WriteString(lipgloss.NewStyle().Padding(0, 2).Render("Some text"))
b.WriteString("\n\n")
b.WriteString(styleActionBar.Render("[q] quit"))

// CORRECT — one newline to end the line, padding adds the blank line
b.WriteString(lipgloss.NewStyle().Padding(0, 2).Render("Some text"))
b.WriteString("\n")
b.WriteString(styleActionBar.Render("[q] quit"))
```

---

## Table Formatting

Tables use `charm.land/bubbles/v2/table` with shared helpers from `tablehelpers.go`.

### Table Styles (`bossTableStyles()`)

- **Header**: bold, faint, left-only padding `Padding(0, 0, 0, 1)`
- **Cell**: left-only padding `Padding(0, 0, 0, 1)` — tighter than default
- **Selected**: bold text with `Foreground(colorSelected)` (`#A8B1F4` periwinkle) — **no background highlight**

### Cursor Column

Every TUI table includes a narrow first column (`cursorColumn`, width 1) that displays `❯` (U+276F) on the selected row and is blank on all others:

```go
cols := []table.Column{
    cursorColumn,  // {Title: " ", Width: 1}
    {Title: "NAME", Width: nameWidth},
    // ...
}
```

When building rows, prepend the indicator:

```go
cursor := m.table.Cursor()
for i, item := range items {
    indicator := ""
    if i == cursor {
        indicator = "❯"
    }
    rows[i] = table.Row{indicator, item.Name, ...}
}
```

After forwarding key events to the table, update the cursor column:

```go
// Forward navigation keys to the table.
var cmd tea.Cmd
m.table, cmd = m.table.Update(msg)
updateCursorColumn(&m.table)
return m, cmd
```

### CLI Tables (No Cursor)

CLI output tables (`handlers.go`) use `WithFocused(false)` and their own styles. They do **not** include `cursorColumn` and do **not** call `updateCursorColumn`. CLI tables use `Padding(0, 1)` for both left and right padding.

### Creating Tables

Use `newBossTable()` for TUI tables (focused, with boss key map and styles):

```go
m.table = newBossTable(cols, rows, height)
```

### Column Widths

Use `maxColWidth(header, values, cap)` to compute widths from data:

```go
cols := []table.Column{
    cursorColumn,
    {Title: "NAME", Width: maxColWidth("NAME", names, 30)},
    {Title: "PATH", Width: maxColWidth("PATH", paths, 60)},
}
```

Use `columnsWidth(cols)` for total rendered width (accounts for left-only padding).

### Table Height

Each view provides a `tableHeight()` method that calculates available height:

- `WithHeight(h)` renders 1 header line + (h-1) data rows
- For CLI showing all rows: `WithHeight(len(rows) + 1)`
- For TUI: compute from terminal height minus overhead (banner, title, action bar)
- Cap at `len(items) + 1` so the table doesn't expand beyond content

### Pre-styled Cell Content

Use `fgOnly()` to wrap pre-styled ANSI content (status colors, etc.) so that inline resets don't clobber the bold attribute on the selected row:

```go
ciStyled := fgOnly(lipgloss.NewStyle().Foreground(ciColor).Render(ciLabel))
```

### ID Columns

Truncate UUIDs to `shortIDLen` characters, no ellipsis:

```go
shortID := item.Id
if len(shortID) > shortIDLen {
    shortID = shortID[:shortIDLen]
}
```

### Key Map

Use `bossKeyMap()` which removes `"d"` from HalfPageDown to avoid conflicts with delete bindings. Only `ctrl+d` remains for half-page-down.

---

## Select Lists (Non-Table)

For simple option lists (not tables), use this pattern:

```go
for i, opt := range options {
    cursor := "  "
    if i == m.cursor {
        cursor = "❯ "
    }
    line := cursor + opt.Label
    if i == m.cursor {
        line = styleSelected.Render(line)
    }
    b.WriteString(lipgloss.NewStyle().Padding(0, 2).Render(line))
    b.WriteString("\n")
}
```

---

## Error Views

Use `renderError()` (defined in `home.go`) to render errors that wrap to the terminal width instead of truncating:

```go
return tea.NewView(
    renderError(fmt.Sprintf("Error: %v", m.err), m.width) + "\n" +
        styleActionBar.Render("[esc] back"),
)
```

Every model that displays errors must track `width int` and handle `tea.WindowSizeMsg`:

```go
case tea.WindowSizeMsg:
    m.width = msg.Width
    return m, nil
```

The `App` must propagate width to child views on resize and when switching views.

Note: `renderError` applies `styleError` with `Width(width - 4)` to account for padding. The `"\n"` between error and action bar is correct here because the error style's padding and action bar's padding together produce exactly one blank line.

---

## Confirmation Dialogs

Use `[y/enter] confirm  [n/esc] cancel` for all yes/no confirmation prompts. Accept both `y`/`Y`/`enter` to confirm and `n`/`N`/`esc` to cancel:

```go
func (m Model) updateConfirm(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
    switch msg.String() {
    case "y", "Y", "enter":
        // proceed
    case "n", "N":
        // go back
    }
    return m, nil
}
```

The `esc` key is handled globally (outside the step switch) so it doesn't need a separate case.

---

## Key Bindings in Action Bar

Format: `[key] action` separated by two spaces.

```
[n]ew session  [r]epo  [enter] select  [q]uit
[a] add  [d] remove  [esc] back
[enter] select  [esc] cancel
[y/enter] confirm  [n/esc] cancel
```

- Highlight the key letter inside brackets for single-char keys: `[n]ew`, `[r]epo`, `[q]uit`
- Use full key name for special keys: `[enter]`, `[esc]`, `[ctrl+d]`
- Combine keys with `/` when multiple keys do the same thing: `[y/enter]`, `[n/esc]`
- Keep action labels short (1-2 words)

---

## Checklist for New Views

- [ ] Uses `styleTitle` for heading
- [ ] Blank line after heading (`"\n\n"`)
- [ ] Table uses `newBossTable()` with `cursorColumn` as first column
- [ ] IDs truncated to `shortIDLen`
- [ ] Cursor column shows `❯` on selected row via `updateCursorColumn()`
- [ ] Table wrapped with `Padding(0, 1)` in `View()`
- [ ] No extra `\n` before `styleActionBar`
- [ ] Error view uses `renderError(msg, m.width)` + `"\n"` + `styleActionBar`
- [ ] Confirmation dialogs use `[y/enter] confirm  [n/esc] cancel`
- [ ] Action bar key format is `[key] action`
- [ ] Handles `tea.WindowSizeMsg` to set `width`, `height`, and update table dimensions
