# Design: a remote attach that feels local

Status: revision 3 (2026-10-19); Phases 1 and 2 built the same day on branch `native-echo` (merged as PR #59), Phase 0 measured on 2026-10-20 (PR #62, BENCHMARKS.md "Remote interaction"), Phase 3 built on 2026-10-20 on branch `attach-frames` (ARCHITECTURE.md "Screen frames"); Phase 4 remains
(ARCHITECTURE.md, "The Input Field And Local Echo"); Phases 0, 3 and 4 remain.
Scope: `agt attach` to a remote daemon, with typing that is echoed locally but looks exactly like
typing into the agent's own input box, for Claude Code, Codex and pi alike, and with output that
never lags behind a slow link.
Supersedes revisions 1 and 2 of this document (same day) and the composer part of the
2026-10-15 office-hours design.

## The problem, in one measurement

`agt --host sfo attach` from the Mac puts about 300 ms under every keystroke: the byte goes to
the devbox, the agent's TUI redraws its input box, and the redraw comes back. That is the ssh
experience, and it is what the target user left for a tool they wrote themselves. mosh fixes it
for shells with predictive local echo and a server-side screen sent as state rather than as a
byte stream.

| mosh | agtd today | This plan |
|---|---|---|
| Predictive local echo: keys go out at once, the client guesses where the echo will land and underlines it until the server confirms | Nothing | The same idea without the guessing: keys go out at once (batched for at most `flush_after`), and the worker, which holds the screen in libghostty, tells the CLI exactly where the agent's input field is, so the local echo is drawn where the agent will draw it and confirmed against what the agent actually drew (Phases 1-2) |
| Server-side terminal, screen state on the wire, adaptive frame rate | The screen is in the worker, but clients get the raw PTY stream; a lagging client is repainted from a snapshot with a visible clear | Phase 3: screen frames paced by the client's acks; nothing dropped, nothing flashes |
| UDP roaming, instant resume | QUIC path migration, reconnect with backoff, reattach by UID | Kept; measured in Phase 0. Keys typed across an outage are resent only once the agent's field shows which ones landed |
| No scrollback | Native scrollback | Kept: frames say how many rows scrolled off, so the local terminal's scrollback keeps growing |

## What "native" means here

The composer draws nothing the agent would not draw. There is no panel, no title line, no key
legend, and no mode. The user attaches, the agent's screen is there, they start typing, and the
characters appear in the agent's own input box at once: same column, same wrapping, same cursor,
the box growing the way that agent's box grows. The keys reach the agent a round trip later and
it draws the same thing in the same place, so nothing visibly changes when it does. Its own
behaviour while typing (the hint row going away, the `/` menu, `@` completion, a paste
placeholder) happens as it always did, one round trip late, under text that was already there.
Enter is just Enter. If agt cannot be sure it would draw what the agent draws, it draws nothing
and the keys go raw, as today.

Five rules that make that hold:

1. **Know, don't guess.** The worker finds the input field on its libghostty screen (the cursor,
   or the agent's own drawn cursor) and tells the CLI the geometry and the text in it. The CLI
   never parses terminal output.
2. **Echo locally, send continuously.** Keys are sent as they are typed, batched for at most
   `flush_after` (50 ms by default). The local echo is a prediction of what the agent will draw
   once those keys reach it, and only keys every editor handles the same way are predicted:
   printable text, Backspace, Left and Right, and the Alt-Enter newline. Any other key (Tab, Esc,
   Ctrl-C, Up, Down, Home, End, Enter) is sent and not predicted; local echo pauses until the
   agent's rendering is seen again. The agent is always the authority: when its field is observed,
   the CLI finds which of its recent predicted states the agent has reached and repaints only what
   is still in flight; if none matches, it adopts what the agent drew.
3. **The agent's chrome is the agent's.** Borders, rules, prompt glyph, placeholder, hint rows and
   colours are the cells already on screen. When the echo needs more rows, the box grows in the
   agent's direction using the agent's own cells; nothing is invented.
4. **Hand over, never repaint over.** After Enter the local text stays on screen until the agent's
   own rendering replaces it. The user never sees the box empty out and refill.
5. **Not native, then raw.** No field found (a menu, a permission prompt, an unknown program): keys
   pass straight through with no echo. Slower, never wrong.

## Why keys trickle instead of waiting for Enter

Revision 2 held the whole draft locally and sent it as one paste on Enter. Sending keys as they
are typed is better on every axis that was open:

* The agent's behaviour while typing is its own, not a copy: hint rows, the `/` menu, `@`
  completion, Codex's token count, Claude Code's handling of a pasted path. agt never has to
  mimic any of it, and it stays native when the agent changes it.
* Enter is just Enter. No paste encoding to verify per agent, no "paste and CR in one write"
  question, no paste placeholder to avoid, no `Existing` text to continue from.
* A lost connection costs at most `flush_after` plus one round trip of keys, and because the CLI
  keeps the predicted states, the agent's field after the reattach shows exactly which keys
  landed, so the rest are resent safely. Today raw input across an outage is dropped because
  nothing can tell.
* Typing while the agent works needs no special case: keys go, echo is predicted, frames correct.
* What it costs: one agent redraw per batch rather than one per prompt, which is what typing raw
  costs today; and the reconciliation below, which is small because the field is known.

`flush_after` is in `config.toml` (`[composer] flush_after = "50ms"`): `0` sends every key at
once, the maximum is 500 ms, and it batches keys typed within the window rather than waiting for
a pause, so a fast typist's keys travel a few at a time like fast typing does. It never delays
the local echo.

## What the three agents actually draw

Measured on 2026-10-19 with a probe that runs each agent under a real PTY at 120x40, feeds the
output through libghostty as the worker does, and dumps the cells, the cursor and the attributes
at idle, after typing 176 characters, after an Alt-Enter newline, and after a bracketed paste of
three lines (`internal/worker/testdata/probe_field.go`; nothing was submitted to a model).

| | Claude Code 2.1.302 | Codex 0.160.0 | pi 0.72.1 |
|---|---|---|---|
| Screen | alternate | alternate | primary |
| Box | a `─` rule above and below, status rows under it | bare prompt row, status rows two below | a coloured `─` rule above and below, status rows under it |
| Prompt glyph | `❯` at column 0 | `›` (bold) at column 0 | none |
| Text starts at | column 2 | column 2 | column 0 |
| Placeholder when empty | faint "Try "edit <filepath> to..."" | faint "Ask Codex to do anything" | none |
| Terminal cursor | visible, in the field | visible, in the field | hidden; pi draws one inverse-video cell |
| Wrap | greedy word wrap at 118 columns (120 minus 2) | the same | greedy word wrap at 120 |
| Continuation rows | indented 2 (under the text, not the glyph) | the same | not indented |
| Box grows | upward: the rule and the rows above move up, status rows stay | upward, status rows stay | downward: the lower rule and status rows move down |
| Alt-Enter (`ESC CR`) | newline | newline | newline |
| Shift-Enter (kitty `CSI 13;2u`) | newline | newline | newline |
| Ctrl-J (kitty `CSI 106;5u`) | newline | newline | nothing |
| Ctrl-Enter (kitty `CSI 13;5u`) | submits | newline | nothing |
| Backspace at a line start | joins the lines | joins | joins |
| Three pasted lines | shown as three lines, no placeholder | the same | the same |
| Typed text style | default pen | default pen | default pen |
| Visible change while text is present | the "← for agents" hint goes away | "? for shortcuts" goes away | nothing |
| OSC 7501 | reports | no | no |

Two things fall out of the table. First, one generic rule describes all three fields: the text
starts where the cursor is, the width is the screen width minus that column, continuation rows
start at that same column, and the box grows upward on the alternate screen and downward on the
primary screen. Second, the agents' reactions to typing (the hint row, the placeholder) do not
need mimicking, since the keys reach the agent anyway; they appear one round trip after the key,
under text that is already on screen.

The probe also found what breaks detection, which is what should break it: a menu (Claude Code's
effort prompt, Codex's folder-trust prompt) hides the terminal cursor or moves it out of any text
field, so no field is found and keys go raw with no echo, which is right for a menu.

## The field model

The worker answers "where would typed text land right now, and what is there" from the active
screen:

```text
Field {
  Found        the rules below matched
  Screen       primary or alternate
  Row, Col     the insertion point: the cursor cell
  TextCol      first column of text on the field rows
  Width        Cols - TextCol
  Indent       TextCol, for continuation rows
  Growth       up (alternate screen) or down (primary screen)
  CursorKind   terminal (the cursor is visible) or inverse (the agent paints a 1-cell inverse block)
  Rows         the text of each field row after the indent, top to bottom (what the agent has drawn)
  Style        SGR bytes for typed text (the default pen plus the row's background, if any)
  Chrome       the field rows and the rows the box may grow into, as VT bytes with their indexes,
               so the CLI can grow the box and restore the rows (Phase 2; frames make this unnecessary)
}
```

Detection, in order: a visible terminal cursor on a row whose text, read back from the cursor,
begins at the first non-blank column after an optional prompt glyph (`❯ › > $`) or at column 0;
otherwise a single inverse-video cell in the bottom half of the screen at the end of a text run
(pi's cursor), never a whole inverse row (a menu's highlight). A session that reports program
status (OSC 7501) must also be `idle`, `done` or `working`, never `blocked`.

**Reconciliation.** The CLI keeps the keys it has sent that the agent has not yet been seen to
draw, each with the predicted field rows after it: S0 (before any of them), S1, ... Sn (what is
on screen now). Each observation of `Field.Rows` (a probe answer in Phase 2, every frame in
Phase 3) is compared with Sn, Sn-1, ... S0. A match at Si confirms the keys up to i; they leave
the queue, and the keys after i are what the echo still owes. No match means the agent did
something the CLI did not predict (a key it does not model, a wrap it computes differently, a
popup that ate a key): the CLI adopts the observed rows, drops the queue, and stops predicting
until the queue is empty again. The queue is bounded (512 keys, 5 seconds): past that the echo
pauses and typing is simply raw, as it is today. Comparing rows rather than one string makes a
real newline and a wrap distinguishable, and makes the agent's own wrapping the reference.

Per-agent profiles exist only as overrides to the detection rules (keyed by the reported app or
the foreground command) and are empty for all three agents today. What makes the model safe is
not the profiles but the fixtures: the probe's recordings of each agent at idle, after typing and
after pasting live in `internal/worker/testdata`, the detector is tested against them, and the
CLI's prediction for the typed keys over the idle screen is compared cell for cell with the
agent's own rendering of the same keys. When an agent changes its box, that test fails and the
fixture is re-recorded, which is how the "identical to the agent" claim stays true across agent
releases.

## Architecture in one picture

```text
agt attach (pure Go)                              agtd session-worker (Go + libghostty)
┌───────────────────────────────────┐             ┌────────────────────────────────────────┐
│ keys, batched ≤ flush_after ────► │ AttachInput │ PTY ◄── input queue, one write per batch│
│ local echo drawn in the agent's   │             │ libghostty shadow terminal              │
│ own box, predicted from the Field,│ ◄── Field ──│   field detection on the cells          │
│ reconciled against Field.Rows     │             │                                         │
│                                   │             │                                         │
│ painter: rows cache, synchronized │ ◄── frames ─│   frame builder: formatter rows diffed  │
│ output, echo composed over live   │ ── acks ──► │   per attachment, one frame in flight,  │
│ frames (Phase 3)                  │             │   Field in every frame (Phase 3)        │
└───────────────────────────────────┘             └────────────────────────────────────────┘
```

The CLI never parses VT output: it receives rows as the VT bytes libghostty's formatter already
produces for snapshots, plus the Field's column numbers and row texts, and paints with absolute
positioning. The worker never crosses the FFI boundary per cell on the hot path: a frame is one
formatter run (a screen-only snapshot of a 160x48 session is about 2 KiB and completes in 0.13 ms
on loopback, BENCHMARKS.md), and field detection reads a few rows' packed cells in one call each.

## Phases

Every phase ships working software and is usable before the next starts. Estimates are
Claude-Code-assisted days.

### Phase 0: measure (1-2 days)

Numbers first, on the link that matters (the Mac to `sfo`, about 300 ms; `omarchy` over
Tailscale), before and after each phase. Done on 2026-10-20: the numbers and what follows from
them are in BENCHMARKS.md under "Remote interaction". In short: a key's echo takes 264 ms on
`sfo` (mosh's round trip there is 206 ms), its confirmation by the field 547 ms, Claude Code
redraws 172 B per key, a 200 KB burst and a streaming answer need no resync, and a 20 s black
hole costs 15 s to notice and 0.7 s to recover from.

* `agt bench echo [HOST/]SESSION` (a hidden developer command, pure Go, reusing the echo markers
  in `internal/bench/measure.go`): attaches to a session running `cat`, types markers and reports
  the keystroke-to-echo distribution. Today's number, mosh's on the same droplet, and later the
  time from a key to its confirmation by the agent's field.
* Time to a correct screen after a 200 KB burst (how many resyncs today), bytes per second during
  a streaming Claude Code answer (the baseline frames must beat), resume time after a 20 s black
  hole, and the agent's output per keystroke (what a redraw costs on the wire; sets `flush_after`).
* `internal/transport/transporttest`'s relay gains a one-way delay knob beside its loss and
  black-hole ones, so every later test runs at 150 ms each way on loopback.
* Results go into BENCHMARKS.md under "Remote interaction".

### Phase 1: the field (3-4 days)

* `internal/worker/field.go`: detection as above, on the owner goroutine from the render state
  (`RenderState.Cursor`, `RowIterator.CellsRaw`, which reads a row's packed cells in one call).
* Fixtures: `record.go` gains `claude-idle`, `codex-idle` and `pi-idle` scenarios (the probe's
  steps, including the first-run dialogs it had to answer), recorded as streams like the existing
  ones, with the typed, newline, Backspace-across-a-wrap and pasted states as further cut points.
  Tests assert the Field for each, and the "detect nothing" cases (the two menus). A shell with a
  visible cursor at a prompt is a field too, and that is fine.
* Capability `attach-field` (attach stream): request `AttachField` → response `Field`. The
  answer rides the attach stream behind the output, so it describes the screen the client has.
* Nothing user-visible yet; `agt status --field` prints it for debugging.

### Phase 2: native echo (about 1.5 weeks)

The user-visible change, on today's output stream.

* **Keys go, echo stays local.** With a Field in hand, each key is appended to the outgoing batch
  (flushed as one `AttachInput` after at most `flush_after`) and, if it is one of the predicted
  keys, applied to the local model and drawn: from `Col` in `Style`, wrapping at `Width` with
  `Indent`, with the cursor as `CursorKind` says (moving the terminal cursor, or painting one
  inverse cell). A bracketed paste from the local terminal is forwarded as a bracketed paste and
  predicted as inserted text, so the agent's own paste handling applies.
* **Growth.** When the echo needs more rows, the box grows in `Growth`'s direction: new interior
  rows take the field row's chrome (its non-text cells, blank between), the rule moves with the
  box, and covered rows are restored from `Field.Chrome` when the text shrinks. Rows beyond the
  box are covered, not shifted, in this phase (Phase 3 shifts them from its cache).
* **Confirmation.** While keys are unconfirmed, output is held (the CLI keeps reading and drops
  it, as the overlay does today) so the agent's redraws cannot fight the echo. 150 ms after the
  last flush the CLI asks for the Field; the answer is reconciled as above. Once the queue is
  empty the screen is repainted once from a snapshot inside synchronized output (DEC 2026, atomic
  on Ghostty, kitty, WezTerm, iTerm2, foot and Alacritty), which shows the agent's own rendering,
  identical in the field and updated around it (the hint row, the spinner), and the hold ends.
  A probe that still shows keys in flight is asked again after the next flush or 150 ms. The one
  visible compromise of this phase: while keys are in flight the screen behind the echo is still,
  for about a round trip after each pause in typing. Phase 3 removes it.
* **Enter and the rest.** Enter, Tab, Esc, Ctrl-C, Up, Down and every other key go out in the
  batch unpredicted; the local text stays on screen and the hold ends at once, so the agent's
  output (the box emptying into the transcript, a menu opening, a completion) replaces it in
  place. Nothing is ever shown by agt on success.
* **Outages.** On a lost connection the queue is kept. After the reattach the CLI asks for the
  Field, matches it against the states as usual, and resends only the keys after the match; if
  no state matches (the agent moved on, or the field is gone) the keys are dropped and the
  bottom-row status says how many, as the reattach status does today. Nothing is resent without
  a match.
* **Configuration.** `[composer] enabled = true`, `flush_after = "50ms"`, `predict = true`
  (false sends keys as today with no echo), and `[composer.<app>]` overrides for the Field rules.
  No key legend, because there are no new keys: the agent's own apply.

Tests: the editor model and wrapper against the probe's fixtures (the prediction for the typed
keys over the idle screen equals the agent's own cells for all three agents, including Backspace
across a wrap and Alt-Enter); reconciliation units (match at each Si, no match, bounded queue,
a wrap the agent computes differently); growth and shrink restore the covered rows exactly
(replay the painter's bytes into a libghostty terminal in `internal/worker` tests and compare
screens); batching and ordering of `AttachInput`; an end-to-end test in `cmd/agt` against a
script agent that draws a prompt, shows a cursor and echoes keys with a configurable delay,
locally and over the relay at 150 ms each way; the reconnect test with keys in flight (only the
unlanded ones are resent); and the manual matrix below against the real agents.

### Phase 3: screen frames (2-3 weeks)

The mosh part. A client that asks for `attach-frames` receives the screen as frames instead of
`PtyOutput`, the echo is composed over live output, and lag never flashes.

Worker:

* Per attachment, on the owner goroutine: a cache of the VT bytes last sent for each screen row
  and the sequence of the last acked frame. One frame in flight per attachment; the next is built
  when the previous is acked and something changed, with a 4 ms floor. The frame rate follows the
  link's round trip and the client's drain rate; a slow client costs one formatter run per round
  trip and one row cache. This replaces drop-and-resync for frame clients (ARCHITECTURE.md's
  backpressure table gains the row).
* A frame: one formatter run over the active screen, split at the CRLFs the formatter puts
  between rows (`formatScreen` already relies on that), rows compared with the cache. Contents:
  `Seq`, `Full`, `Screen`, `Scrolled` (rows the primary screen pushed into history since the last
  frame, from `ScrollbackRows` with mosh's row-shift comparison as the check at the scrollback
  cap), cursor, the formatter's mode prefix when it changes, colour changes, effects (bell, title,
  notifications, clipboard writes), `Field` (detection runs per frame, so reconciliation happens
  on every frame and Phase 2's probe goes away), and `Rows []{Index, VT}`.
* Attach under frames: `Attached.Snapshot` carries only the history rows (so native scrollback
  starts full), then the first `AttachFrame{Full}`. Reattach, resync and overlay close all become
  "send a full frame"; every frame is an exact boundary (`doAtGround`).

Client (`internal/screen`, pure Go, used by `internal/cli`; its tests in `internal/worker` replay
its bytes into a libghostty terminal and compare cells):

* A painter with its own row cache, cursor and modes. Each frame is one write inside synchronized
  output: `Scrolled` newlines at the bottom row first, so the local terminal pushes exactly the
  rows that left the agent's screen into its own scrollback, then each changed row by absolute
  position, then the cursor. No clear, no flash.
* The echo composes over frames: while keys are unconfirmed, the field rows (and the rows the box
  grew into) are drawn from the local model over the frame; everything else is live (the spinner,
  the transcript, the hint row changing). Each frame's `Field.Rows` reconciles the queue; once it
  is empty the frame's own rows show through, identical. Growth now shifts the rows above (or
  below) as the agent would, from the cache. The held-output compromise of Phase 2 is gone, and
  so is the one-snapshot repaint.
* The painter pushes the agent's kitty flags and mouse modes to the local terminal when the
  frame's mode prefix changes and pops them on detach. Keys still go as `AttachInput`.
* `[attach] mode = "frames" | "raw"`; frames when the worker offers them, raw for older workers,
  `agtd bench`, and sessions that use kitty graphics or sixel (not painted; the frame says so).

Measured before merge, on the delayed and lossy relay and on `sfo`: time from the agent's write to
the painted row (target: one round trip plus a frame), bytes per second against the raw baseline
during a Claude Code answer and during typing, worker CPU during a flood with one and three
attachments, memory per attachment, and frames against raw on the local Unix socket (if the
formatter run per batch costs more than the fan-out it replaces, local attachments default to raw).

Tests (CLAUDE.md's list, under frames): attach, detach, reattach after a cut, two attachers at
different drain rates, resize, alternate-screen switch, scroll pass-through into a libghostty
terminal's history, a client that stops reading while the session writes 67 MB (worker memory
flat, no drops counted), the recorded streams painted through the painter cell for cell against
the shadow terminal, a lost connection mid-frame, daemon restart and live handoff under frames,
echo over frames with the script agent at 150 ms each way, and fuzzing of the frame decoder.

### Phase 4: finishing (about 1 week)

* Submit and switch (`Ctrl-Y, Enter`): send Enter and jump to the next session that needs
  attention.
* Capability `attach-compose` and `agt send-input --compose`: a whole prompt as one request,
  encoded by the worker from the terminal's own modes (libghostty `PasteEncode` with `bracketed`
  from mode 2004, Enter through a `KeyEncoder` set with `SetOptFromTerminal`), acknowledged once
  queued. For scripts and `agt events --exec` hooks; the interactive path never needs it.
* A status line stays off by default; it is not native. The Ctrl-Y palette stays as it is: it is
  agt's own layer, like a multiplexer prefix, and is never shown unasked.

### Documentation, at each phase

README (the attach section gains "Typing is local", with `flush_after`; the limitations list),
ARCHITECTURE ("Wire Protocol" tables, "The input field", "Screen frames" beside "The reattach
snapshot", the backpressure table), ROADMAP (a phase for this plan), BENCHMARKS ("Remote
interaction").

## Protocol additions (all under capabilities; golden frames and fuzz for each)

| Capability | Stream | Messages | Phase |
|---|---|---|---|
| `attach-field` | attach | `AttachField` (kind 52) → `Field` (kind 152) | 1 |
| `attach-frames` | attach | `AttachFrame{Seq, Full, Screen, Scrolled, Cursor, ModePrefix, Colors, Effects, Field, Rows}` (kind 153); `AttachFrameAck{Seq}` (kind 53); `Attached.Snapshot` holds history rows only | 3 |
| `attach-compose` | attach, request | `AttachCompose{Seq, Text, Submit}` (kind 54) → `AttachComposed{Seq}` (kind 154) or `Error` | 4 |

Kinds 50-59 and 150-159 are the attach-stream range. `AttachSession.Features` already carries
per-stream features and the daemon cuts the list to what the session's worker supports, so an
older worker serves raw output, and raw keys, to a newer CLI.

## Explicitly not in this plan

* A composer panel of agt's own (revision 1), or a draft held locally until Enter (revision 2).
  When no field is found, keys go raw; agt never shows a box the agent did not draw, and never
  keeps the agent from seeing what was typed.
* Predicting keys whose effect differs between editors (Home, End, Ctrl-A/E/K/U/W, Up, Down): they
  are sent and their effect appears a round trip later, as it does today.
* Vendor channels rendered locally (pi's JSONL RPC, Claude Code stream-json, Codex app-server), a
  phone or browser surface, relays, NAT traversal, QUIC datagrams, and resizing the PTY to make
  room for anything.

## Open questions, each settled by a test before the phase that needs it

1. **Which keys are deterministic.** Do all three editors treat Backspace at a wrap, Left and
   Right across a wrap, and Alt-Enter in the middle of a line the same way the CLI's model does?
   The Phase 1 fixtures type those sequences and the fidelity test compares cells; a key that
   differs on any agent is moved to the unpredicted list.
2. **The flush window.** Phase 0 measures what one agent redraw costs on the wire and on the
   devbox; 50 ms is the default unless Claude Code's per-key rendering turns out heavy, in which
   case 100 ms. Either way the echo is instant. Settled: 172 B per key on `sfo`, 69 B locally;
   50 ms stays.
3. **Chunks taken for pastes.** With a 50 ms window a fast typist's keys arrive a few at a time;
   does any agent treat an unbracketed chunk of several characters as a paste (Claude Code has a
   heuristic for that)? Test 10-character bursts; the window shrinks, or batches are cut at a
   size, if so.
4. **Probe timing in Phase 2.** The worker answers `AttachField` as soon as it reads the
   request, which may be before the agent has drawn the keys written just before it. A stale
   answer matches an earlier state and is harmless (the echo owes more), but it costs another
   probe; measure how often on `sfo` and whether a 150 ms wait is the right one.
5. **Typing while the agent works.** Claude Code queues a message typed during a turn; what do
   Codex and pi do, and does the field stay detectable (cursor still in the box) while they work?
   Decides whether `working` keeps the field for agents that do not report status.
6. **Cheapest row formatting** for frames: one full-screen run plus a split at CRLFs, or one run
   per changed row over a one-row selection? Benchmark both on the recorded streams; a 200x50
   frame should stay under 0.2 ms.
7. **Scroll detection at the scrollback cap** and **width mismatches** in frames: the row-shift
   comparison and per-row absolute positioning, measured on the recorded streams.

## Manual matrix (Phase 2 acceptance, repeated per agent release)

For each of Claude Code, Codex, pi and a shell, over `sfo`:

1. Attach, type a one-line prompt: every character appears at once, in the agent's box, where
   the agent would put it; a pause later the agent's hint row changes under it, and nothing in
   the field moves. Enter: the agent answers, and the box did not empty and refill.
2. A four-line prompt with Alt-Enter: continuation rows indented as the agent indents them; the
   box grew in the agent's direction; after Enter the transcript shows the four lines.
3. A prompt that wraps three times; Backspace across the wrap; Left across the wrap and type in
   the middle; Enter.
4. `/` on an empty box: the `/` is echoed at once and the agent's menu opens a round trip later;
   arrows and Enter work it with no echo of their own.
5. `@src/` then Tab (Claude Code): the completion appears a round trip later.
6. Ctrl-C with text clears the box a round trip later; Ctrl-C again does what the agent does.
7. Type while the agent is working (where question 5 allows it): the message queues.
8. Pull the network for 20 s while typing: the status row reports, the reattach completes, the
   field is compared, only the keys that did not land are resent, and the text reads as typed.
9. The two menus (effort, folder trust) and a permission prompt: no echo, keys go raw.
10. A shell session: typing is echoed locally too, Enter runs the command.

## What changed from revision 2

* Keys are sent as they are typed (batched by `flush_after`), and the local drawing is a
  prediction reconciled against the agent's own field rows, rather than a draft held until Enter
  and sent as a paste. The paste encoding, the placeholder question and the flush-then-raw rules
  go away; reconciliation (a short history of predicted states) comes in.
* The Field carries the agent's field rows (`Rows`) for reconciliation, and frames carry the
  Field, so under frames every frame confirms the echo.
* Keys typed across an outage are resent only for the part the agent's field proves did not
  land; revision 2 could only keep a draft.
* `attach-compose` moved from a phase of its own to the finishing phase, for scripts only.
* Drafts on disk are gone: there is no local draft, the text is at the agent.

## Order of work and checkpoints

```text
Phase 0  measure              numbers in BENCHMARKS.md; the per-key redraw cost sets flush_after
Phase 1  field detection      fixtures from the three agents; attach-field
Phase 2  native echo          tagged release; the Rowan trial can run here
Phase 3  attach-frames        live screen under the echo, no flash, scrollback kept; measure again
Phase 4  submit-and-switch, attach-compose for scripts
```

Each phase is one PR series behind a capability, lands with its tests and documentation, and
leaves older CLIs and daemons working as they do today.
