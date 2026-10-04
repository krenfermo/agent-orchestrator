#!/bin/sh
# select_tui.sh — a minimal terminal select prompt with the SAME semantics as
# Claude Code's: a cursor that moves on Up/Down, and Enter that confirms
# whichever row the cursor is currently on.
#
# It exists so P3-C's structured-response E2E runs against a real terminal doing
# real cursor movement. A fixture that could not confirm the WRONG option could
# not prove that AO confirms the right one, and that is the only property this
# capability has.
#
# Raw mode with echo off is what a real TUI sets up for itself, and it is
# load-bearing here: without it the terminal line-buffers input and echoes the
# arrow escape as "^[[B" into the pane instead of delivering it. Every line is
# written with an explicit CR+LF because raw mode does no translation.
#
# $2 selects the LAYOUT, so one fixture can render the shapes AO has to survive
# against a real terminal (P3-D §18):
#
#   (default)     one column, the pre-P3-D shape;
#   two-column    options on the left with a code-preview box drawn beside them,
#                 which is what Claude Code 2.1.258 actually renders and what
#                 AO's parser used to read as "no prompt on screen";
#   partial       a half-drawn frame that never completes, so a reader sees a
#                 prompt's furniture and an unparseable list — the shape that
#                 must be INCONCLUSIVE and must not attract a keystroke.
#
# The confirmed option (1-based) is written to $1.
OUT="$1"
LAYOUT="${2:-single}"
CURSOR=1
N=4
stty raw -echo 2>/dev/null
restore() { stty sane 2>/dev/null; }
trap restore EXIT

emit() { printf '%s\r\n' "$1"; }

# render_single is the original one-column shape, unchanged.
render_single() {
  emit "What should the new helper file be named?"
  i=1
  for opt in "pathutil.go" "pathhelpers.go" "Type something." "Chat about this"; do
    if [ "$i" = "$CURSOR" ]; then emit "❯ $i. $opt"; else emit "  $i. $opt"; fi
    emit "     Choose $opt"
    i=$((i+1))
  done
  emit "Enter to select"
}

# render_two_column pads every option row to a fixed width and draws a box to
# its right, on the SAME physical lines. That sharing is the whole point: a
# reader that takes a line at face value gets the box in its option labels, and
# the box's own continuation rows look like output printed after the list.
render_two_column() {
  emit "What should the new helper file be named?"
  i=1
  for opt in "pathutil.go" "pathhelpers.go" "Type something." "Chat about this"; do
    if [ "$i" = "$CURSOR" ]; then LEFT="❯ $i. $opt"; else LEFT="  $i. $opt"; fi
    case "$i" in
      1) RIGHT="┌──────────────────────────────┐" ;;
      2) RIGHT="│ // path helpers live here    │" ;;
      3) RIGHT="│ func Join(a, b string) {}    │" ;;
      4) RIGHT="└──────────────────────────────┘" ;;
    esac
    printf '%-34s%s\r\n' "$LEFT" "$RIGHT"
    i=$((i+1))
  done
  emit "                                  Notes: press n to add notes"
  emit "Enter to select"
}

# render_partial draws the heading and the first option and then stops, leaving
# an unfinished box. Nothing here is a complete list and nothing is an absence.
render_partial() {
  emit "What should the new helper file be named?"
  printf '%-34s%s\r\n' "❯ 1. pathutil.go" "┌──────────────────────────────┐"
  printf '%-34s%s\r\n' "" "│ // path helpers live here"
}

render() {
  printf '\033[2J\033[H'
  emit "* I need to ask you a question before writing anything."
  emit "--------------------------------------------"
  case "$LAYOUT" in
    two-column) render_two_column ;;
    partial)    render_partial ;;
    *)          render_single ;;
  esac
}

render
while :; do
  KEY=$(dd bs=1 count=1 2>/dev/null | od -An -tu1 | tr -d ' \n')
  [ -z "$KEY" ] && continue
  case "$KEY" in
    27) # ESC introducer: consume '[' and then the direction byte
      dd bs=1 count=1 >/dev/null 2>&1
      DIR=$(dd bs=1 count=1 2>/dev/null | od -An -tu1 | tr -d ' \n')
      case "$DIR" in
        65) [ "$CURSOR" -gt 1 ] && CURSOR=$((CURSOR-1)) ;;    # Up
        66) [ "$CURSOR" -lt "$N" ] && CURSOR=$((CURSOR+1)) ;; # Down
      esac
      render
      ;;
    10|13) # Enter confirms the CURSOR, faithfully
      printf '%s' "$CURSOR" > "$OUT"
      restore
      printf '\033[2J\033[H'
      echo "selected $CURSOR"
      exit 0
      ;;
  esac
done
