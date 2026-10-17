#!/usr/bin/env bash
# capture_claude_composer_styled.sh refreshes claude_composer_*_styled_rendered.txt:
# Claude composer rows drawn into a real tmux pane with the SGR sequences Claude
# Code 2.1.302 emits for them, then read back with capture-pane -e, so the
# fixtures carry tmux's re-encoding of those bytes. No Claude session runs.
#
# The sequences are read from the 2.1.302 bundle. A placeholder or a prompt
# suggestion renders as invert(p[0]) + dim(p[1:]) (the Je renderer); the cursor
# after typed text is an inverted space; the IDE selection pill is
# "[⧉ <label>] " in the theme's "ide" colour, rgb(71,130,200), drawn ahead of
# the placeholder or the typed text. A completion after typed text (the
# inlineGhostText prop) is dim, with the cursor inverted on its first letter.
# The rules use promptBorder, rgb(136,136,136); the footer is plain.
#
# Under NO_COLOR the bundle's ne() sets the colour level to 0, which drops
# every colour and the dim, but qte, the inverse the cursor uses, is
# unconditional. The no_color_* fixtures draw those bytes.
#
# The committed fixtures were captured on 2026-10-18 with tmux 3.7c.
set -u
L=capture-claude-composer-styled
OUT_DIR=$(cd "$(dirname "$0")" && pwd)
WORK=$(mktemp -d)
trap 'tmux -L $L kill-server 2>/dev/null; rm -rf "$WORK"' EXIT

E=$'\033'
PLAIN_RULE=$(printf '─%.0s' $(seq 1 100))
RULE="${E}[38;2;136;136;136m${PLAIN_RULE}${E}[39m"
MARKER="${E}[38;2;153;153;153m❯${E}[39m "
PILL="${E}[38;2;71;130;200m[⧉ 3 lines from main.go] ${E}[39m"
PILL_BEFORE_TEXT="${E}[38;2;71;130;200m[⧉ 3 lines from main.go]${E}[39m "
PLACEHOLDER="${E}[7mT${E}[27m${E}[2mry \"fix typecheck errors\"${E}[22m"
SUGGESTION="${E}[7mc${E}[27m${E}[2mheck on the build${E}[22m"
DRAFT="half-written draft${E}[7m ${E}[27m"

# capture NAME ROW [RULE MARKER]
capture() {
  name=$1
  row=$2
  rule=${3:-$RULE}
  marker=${4:-$MARKER}
  {
    printf '⏺ The tests pass.\r\n\r\n'
    printf '%s\r\n%s%s\r\n%s\r\n' "$rule" "$marker" "$row" "$rule"
    printf '  ⏵⏵ bypass permissions on (shift+tab to cycle)\r\n'
  } >"$WORK/$name"
  tmux -L $L -f /dev/null new-session -d -s "$name" -x 100 -y 8 "stty raw -echo; cat '$WORK/$name'; exec sleep 60"
  sleep 0.5
  tmux -L $L capture-pane -p -e -t "$name" >"$OUT_DIR/claude_composer_${name}_styled_rendered.txt"
}

capture placeholder "$PLACEHOLDER"
capture suggestion "$SUGGESTION"
capture draft "$DRAFT"
capture ide_pill_placeholder "$PILL$PLACEHOLDER"
capture ide_pill_draft "$PILL_BEFORE_TEXT$DRAFT"
# capture-pane -e carries the style across a row the terminal wrapped, so the
# second row of a wrapped placeholder starts with no SGR at all.
capture wrapped_suggestion "${E}[7mc${E}[27m${E}[2mheck on the build, then summarise every failing test, the commit that broke it, and who to ask about it${E}[22m"
# A dim row that fills the width above a plain draft row: tmux opens the
# second row with the reset that ends the dim.
capture dim_row_above_draft "${E}[2m$(printf 'g%.0s' $(seq 1 98))${E}[22m"$'\r\n'"  real draft${E}[7m ${E}[27m"
# An operator's partial slash command with Claude's completion drawn after it.
capture slash_completion "/boss-re${E}[7mp${E}[27m${E}[2mair${E}[22m"
capture no_color_placeholder "${E}[7mT${E}[27mry \"fix typecheck errors\"" "$PLAIN_RULE" "❯ "
capture no_color_draft "half-written draft${E}[7m ${E}[27m" "$PLAIN_RULE" "❯ "
