# Terminal UX: scrollback, selection, copy, and follow

What the work-task terminal (`src/renderer/components/XtermTerminal.tsx`) does
about reading history — how much of it is kept, how selecting and copying it
behaves, and how the viewport treats live output — plus the manual smoke test
that covers the parts no unit test can see.

The rules that can be tested without a live xterm live in
`src/renderer/lib/terminal-follow.ts` (follow / scroll-lock) and
`src/renderer/lib/terminal-themes.ts` (selection colours); everything else is
wired in `XtermTerminal.tsx`.

## Scrollback bound: 5000 lines

`TERMINAL_SCROLLBACK_LINES = 5000` — the value passed as xterm's `scrollback`
option, hard-coded in `XtermTerminal.tsx`.

**Why a bound at all.** xterm retains every scrollback line as a live JS buffer
in the renderer, and AO holds many terminals alive at once: `TerminalPane`'s
cache keeps a mounted surface per handle generation across route switches, so
nothing frees that history until the handle is replaced. Unbounded (or
very large) scrollback therefore grows for the whole life of a long-running
Claude/Codex session and is never reclaimed — across several parked sessions
that is renderer memory the user never gets back.

**Why 5000 and not less.** 5000 lines is roughly 200 screens at a typical
24-row grid. That is enough to scroll back through a long agent session — a
full `git log`, a build log, a Claude turn that printed a large diff — without
the history being trimmed out from under the reader mid-review.

**Why 5000 and not more.** The bound caps one pane's retained history at
single-digit megabytes, which keeps the total bounded even with many panes
parked in the cache. Beyond a few thousand lines the extra history is rarely
read, while the memory cost is paid continuously.

**Where the bound shows up elsewhere.** Because the buffer trims from the top
once it is full, `baseY - viewportY` stops growing while output keeps arriving.
The new-output counter therefore counts line feeds rather than deriving a
distance from the buffer — see `noteLineFeed` in `terminal-follow.ts`. It must
also stay `> 0` for normal-buffer panes, or those panes would have no local
history to scroll at all.

## What the terminal does now

**Selection is visible.** `buildTerminalThemes()` sources
`selectionBackground` / `selectionForeground` / `selectionInactiveBackground`
from `tokens.css`, with distinct dark and light values and literal fallbacks so
a renderer without the stylesheet still gets a high-contrast selection instead
of xterm's translucent default. Selection is forced on
(`forceSelectionMode`, `macOptionClickForcesSelection`) so mouse-tracking panes
can still be selected with the mouse.

**Copy is platform-appropriate and never eats the interrupt.**
`isTerminalCopyShortcut` accepts Cmd+C (macOS), Ctrl+Shift+C, Ctrl+Insert, and
plain Ctrl+C on Windows. Every one of them routes through the single
`copySelection()` path, which returns `false` when `term.getSelection()` is
empty — so a bare Ctrl+C with no selection falls through to the PTY as SIGINT,
unchanged. Selecting also copies (deduped, so re-copying the same range is not
repeated). The right-click menu's Copy, the title-bar Edit menu (via
`lib/terminal-clipboard.ts`, needed because xterm's selection is not a DOM
selection), and the `copy` DOM event all use the same path. Paste
(Cmd+V / Ctrl+Shift+V / Shift+Insert) is untouched, including bracketed-paste
handling.

**Wheel and trackpad scroll stay in the terminal.** The custom wheel handler
consumes the gesture over the terminal (`preventDefault` + `stopPropagation`)
and banks sub-cell trackpad pixels in an accumulator so slow trackpad scrolling
still advances a line at a time. Panes that own their transcript get SGR wheel
reports (tmux) or page keys (keyboard-scroll TUIs like opencode) instead. CSS
backs this with `overscroll-behavior: contain` on `.xterm-viewport`, so no
gesture chains out to the page; the page scrolls normally once the pointer
leaves the terminal. Ctrl/Cmd+wheel is left alone for font-size zoom.

**Follow is derived, never asserted.** `createTailFollowTracker` decides
"following" from the buffer's `{baseY, viewportY}` on every scroll event, with a
2-line slack window (`FOLLOW_TAIL_SLACK_LINES`) so a trackpad flick that lands
one line short still counts as the tail. Scrolling up disables follow; scrolling
back near the bottom re-arms it on its own. `settleAtTail()` snaps to the exact
bottom when the viewport comes to rest inside the slack window, which is what
clears xterm's internal user-scrolling lock.

**New output does not yank the viewport.** While the reader is scrolled up, new
lines land off-screen and are counted (capped at `NEW_LINE_COUNT_CAP = 999`). A
"Jump to latest" / "N new lines" pill appears at the bottom; clicking it scrolls
to the bottom, re-enables follow, and returns focus to the terminal. The strip
around the pill is `pointer-events-none` so wheel and selection drags still
reach the rows underneath.

**Keyboard scrollback.** For panes whose history lives in xterm, PageUp/PageDown
(and macOS Fn+Up / Fn+Down, which report as those) scroll the viewport.
Home/End only become scrollback keys with Shift, or while the reader has already
left the tail — otherwise they stay readline's beginning/end-of-line.

**Resize does not jump.** `fitPreservingViewport()` captures the viewport's
distance from the tail before `fit()` and restores it after: a follower stays
following, a reviewer lands the same distance above the tail. A fit that does
not change the grid is a no-op inside `FitAddon`, which is what keeps an active
selection alive across ordinary layout changes (splitter drags, sidebar
toggles). When the grid does change, the reflow rewrites the lines a selection
is anchored to and xterm exposes no way to remap it — that selection is lost
with or without the anchor.

## Manual smoke test

Run against a real work-task terminal with a live agent (Claude or Codex), in
both dark and light theme.

- [ ] **Selection is visible.** Drag across output. The selected text has an
      obvious high-contrast background and readable foreground. Click elsewhere
      to blur the terminal — the selection stays visible in the inactive
      colour. Check in both dark and light theme.
- [ ] **Cmd+C copies the selection.** With a selection up, press Cmd+C
      (Ctrl+Shift+C on Windows/Linux) and paste elsewhere: the exact selected
      text arrives. Right-click → Copy and the title-bar Edit → Copy give the
      same result.
- [ ] **Interrupt still works with no selection.** With nothing selected, run a
      long command and press Ctrl+C. It interrupts, exactly as in a native
      terminal. (On Windows, do this with no selection — Ctrl+C only copies
      when there is one.)
- [ ] **Trackpad scrolling feels normal.** Two-finger scroll over the terminal
      moves the terminal only. Slow, small gestures still advance line by line.
      The page underneath does not move. Move the pointer off the terminal —
      the page scrolls again.
- [ ] **Long scrollback works.** Print far more than a screen (e.g. `git log`
      or a long build) and scroll back through it. History is there, up to the
      5000-line bound.
- [ ] **Scrolling upward stays put.** Scroll up mid-output. The viewport stays
      where it was put; it does not drift or snap back.
- [ ] **New output does not force scroll down.** While scrolled up with the
      agent still printing, the viewport does not move and an in-progress
      selection is not disturbed. The "N new lines" pill appears and its count
      climbs.
- [ ] **Jump to latest works.** Click the pill: the viewport goes to the
      bottom, the pill disappears, follow resumes (new output now scrolls), and
      the next keystroke goes to the agent rather than to the button.
- [ ] **Resize does not unexpectedly jump.** While following, drag the
      task-panel splitter, resize the window, and toggle the sidebar — the
      terminal stays at the bottom. Repeat while scrolled up: the viewport
      stays at roughly the same place in the history. A selection survives
      layout changes that do not change the cell grid.
- [ ] **Terminal stays stable during live output.** With an agent actively
      printing, the pane does not flicker, reflow oddly, lose focus, or drop
      keystrokes; selecting or copying never focuses outer chrome, triggers a
      task action, or collapses a panel.

## Automated coverage

- `src/renderer/lib/terminal-follow.test.ts` — tail detection, slack window,
  new-line counting and cap, anchor capture/restore.
- `src/renderer/lib/terminal-themes.test.ts` — selection colours present and
  distinct per theme.
- `src/renderer/components/XtermTerminal.test.tsx` — copy shortcuts and
  interrupt fall-through, scroll/follow, the jump-to-latest control, wheel
  handling, and resize behaviour.

Verify with `npm run typecheck`, `npm test`, and `npm run build` in `frontend/`.
