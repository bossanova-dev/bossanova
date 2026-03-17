# TUI Design Guidelines

Reference guide for building consistent Bubbletea v2 views in the boss CLI. Consult this skill when creating or modifying any TUI view.

---

## Layout Constants

All shared layout constants live in `services/boss/internal/views/home.go`:

```go
const (
    shortIDLen    = 7 // characters shown for truncated UUIDs
    colGap        = 2 // spaces between table columns
    actionBarPadY = 1 // blank lines above action bar
)

var colSep = strings.Repeat(" ", colGap)
```

**Always use these constants** — never hardcode `7`, `2`, or `1` for these purposes.

---

## Shared Styles

Defined in `services/boss/internal/views/home.go`:

| Variable         | Purpose                          |
| ---------------- | -------------------------------- |
| `styleTitle`     | Bold heading with horizontal pad |
| `styleSelected`  | Bold text for cursor-highlighted |
| `styleActionBar` | Faint bottom menu with top pad   |
| `styleError`     | Red text with padding            |
| `styleSubtle`    | Faint secondary text             |
| `colorGreen`     | Success / merged / green states  |
| `colorYellow`    | In-progress / pending states     |
| `colorRed`       | Error / blocked states           |
| `colorCyan`      | Transitional states              |
| `colorGray`      | Unknown / default states         |
| `colorOrange`    | Brand / banner accent            |

---

## View Structure

Every view follows this top-to-bottom structure:

```
[heading]          styleTitle.Render("Title") + "\n\n"
[blank line]
[faint header]     table column headers (faint)
[rows]             data rows with "> " / "  " cursor
[action bar]       styleActionBar.Render("[key] action  ...")
```

### Rules

1. **One blank line** between heading and content (`"\n\n"` after `styleTitle`)
2. **No explicit newline before `styleActionBar`** — it has `Padding(actionBarPadY, 2)` built in, which provides the blank line above
3. **Cursor prefix** is always `"> "` (selected) or `"  "` (unselected) — 2 chars wide
4. **Wrap rows** with `lipgloss.NewStyle().Padding(0, 2).Render(row)` for horizontal indent

### Action Bar Spacing

`styleActionBar` already includes top padding via `Padding(actionBarPadY, 2)`. **Never** add an extra `\n` before it:

```go
// WRONG — creates double blank line
b.WriteString("\n")
b.WriteString(styleActionBar.Render("[q] quit"))

// CORRECT — styleActionBar handles spacing
b.WriteString(styleActionBar.Render("[q] quit"))
```

The one exception: if content before the action bar uses a style without top padding (e.g., a confirmation prompt), add `"\n"` manually in that branch only.

---

## Table Formatting

### Column Widths

Compute column widths from actual data, capped at a reasonable max:

```go
maxName := len("NAME")
for _, item := range items {
    if len(item.Name) > maxName {
        maxName = len(item.Name)
    }
}
if maxName > 30 {
    maxName = 30
}
```

### ID Columns

Truncate UUIDs to `shortIDLen` characters, no ellipsis:

```go
shortID := item.Id
if len(shortID) > shortIDLen {
    shortID = shortID[:shortIDLen]
}
```

### Column Separator

Use `colSep` (derived from `colGap`) between columns in format strings:

```go
header := fmt.Sprintf("  %-*s"+colSep+"%-*s"+colSep+"%-*s",
    shortIDLen, "ID", maxName, "NAME", maxPath, "PATH")

row := fmt.Sprintf("%s%-*s"+colSep+"%-*s"+colSep+"%-*s",
    cursor, shortIDLen, shortID, maxName, name, maxPath, path)
```

### Truncation

Use the shared `truncate()` function for content that may exceed column width:

```go
func truncate(s string, max int) string  // adds "..." suffix
```

---

## Select Lists (Non-Table)

For simple option lists (not tables), use this pattern:

```go
for i, opt := range options {
    cursor := "  "
    if i == m.cursor {
        cursor = "> "
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
- [ ] Table columns use `colSep` and computed widths
- [ ] IDs truncated to `shortIDLen`
- [ ] Cursor is `"> "` / `"  "` (2 chars)
- [ ] Rows wrapped with `Padding(0, 2)`
- [ ] No extra `\n` before `styleActionBar`
- [ ] Error view uses `renderError(msg, m.width)` + `"\n"` + `styleActionBar`
- [ ] Confirmation dialogs use `[y/enter] confirm  [n/esc] cancel`
- [ ] Action bar key format is `[key] action`
