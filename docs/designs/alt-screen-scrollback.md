# Design: native scrollback for agents on the alternate screen

Status: revision 4 (2026-10-25). After the prototype's first day on sfo, 199 ms away, the plain
wheel and streaming answers felt far from a local Claude Code: decision 12 now predicts wheel
scrolling of the transcript locally and lets the frames reconcile it, decision 11 keeps a bounded
copy for that, and decision 13 sizes the frame window from the round trip. Revision 3 (2026-10-23): Phase 1 is built on branch `alt-scrollback`: the `claude-scroll`
fixture, the matcher and record (`internal/screen/transcript.go`), and the replay test; its results
are in "Phase 1 results" and change decisions 1 to 4. Revision 2 added decisions 11 and 12 (the
client keeps a cursor, not a copy of the rows; the agent's viewport is not predicted) and open
question 9. Phase 2's client prototype is built on the same branch: `[attach] alt_scrollback`
(off by default) makes the painter stay on the primary screen and scroll recorded rows into the
terminal's scrollback, and the worker now holds a frame while a synchronized-output block is open
(at most 150 ms, `frameSyncHold`), since a frame inside one shows half a repaint, to every frame
client and not only this one. `TestFramesAltScrollbackClaudeScroll` replays the fixture through the
worker's frames and the painter into a client terminal: its own scrollback holds every row once, in
order, at four paces. It answers the question whether a
client-side framebuffer could predict the rows of a Claude Code session and give near-native
scrollback while the screen keeps coming from the worker as frames: yes, and the record belongs in
the worker, with the client's framebuffer doing the painting it already does.
Scope: `agt attach` under screen frames (`attach-frames`, ARCHITECTURE.md "Screen frames") to a
session whose program draws its transcript on the alternate screen (Claude Code, Codex, pi in
full-screen mode), so that rows leaving the top of the program's viewport land in the user's own
terminal scrollback as if the program had printed them on the primary screen, and so that a
reattach hours later seeds that scrollback with the transcript so far.

## The problem

A Claude Code session has no scrollback, locally or remotely. Claude Code 2.1.302 and Codex
0.160.0 run on the alternate screen (`smooth-remote-attach.md`, "What the three agents actually
draw"); so does pi 1.x in its full-screen mode, its default (`tuiMode`; pi 0.7x printed inline on
the primary screen). Claude Code's first bytes are

```text
ESC 7  ESC [r  ESC 8  ESC [?25h  ESC [?2004h  ESC [?2031h  ESC [?1004h  OSC 0;✳ Claude Code
ESC [?1049h  ESC [2J  ESC [H  ESC [?1000h  ESC [?1002h  ESC [?1003h  ESC [?1006h  ESC [?25l
```

and from then on it repaints. In the 47 KB field recording (`internal/worker/testdata/streams/
claude-field.vt.gz`, 120x40, typing and a short answer):

| Sequence | Count | Meaning |
|---|---|---|
| `CSI ? 2026 h` | 668 | synchronized output around every repaint |
| `CSI H` (cursor position) | 1,853 | rows written by absolute position |
| `CSI K` (erase to end of line) | 325 | rows cleared before rewriting |
| `CSI r` | 3 | scroll region reset, never set |
| LF, `ESC D`, `CSI S`, `CSI L`, `CSI M` | 0 | nothing ever scrolls |

So in terminal terms nothing scrolls: a transcript row that moves up the screen is a repaint, and
a row that leaves the top is gone. libghostty keeps no history for the alternate screen, like every
terminal, so the worker's frames never carry `Scrolled` there (`frames_test.go` pins it: "the
alternate screen never scrolls"). The client mirrors `?1049h` from the frame's prefix, so its own
terminal is on the alternate screen too and has nothing to scroll back into; and Claude Code turns
mouse reporting on, so the wheel is sent to the agent, which scrolls its own viewport one round
trip away.

What is missing is what Claude Code on the primary screen used to give for free: scroll up in the
terminal and read the conversation, with the terminal's own search, selection, speed and keys. A
remote attach cannot usually offer what the local terminal does not have. Here it can offer more,
because the worker holds the program's screen in libghostty and sees every write to it, and the
client paints the screen from rows anyway. This is squarely what agtd is for: the session is
durable and observable, and the transcript of a run that happened while the laptop was closed
should be there when the user comes back, not just the last forty rows of it.

## What frames mode already does on the primary screen

Everything the question asked for exists for the primary screen, so this plan extends it rather
than inventing a mechanism.

* The painter (`internal/screen`, pure Go) is a framebuffer: it caches the VT of every row it
  painted, paints changed rows by absolute position inside synchronized output, and mirrors the
  program's modes, keyboard state and cursor.
* A frame says how many rows the primary screen pushed into history since the last frame
  (`Frame.Scrolled`) and carries their final contents (`Frame.History`, at most `frameMaxHistory`
  = 1,000). The painter paints them over its top rows and line-feeds at the bottom row, so the
  terminal pushes exactly those rows into its own scrollback. Rows beyond what a frame carries go
  "as they were": the painter's cached rows are the prediction, and the worker's history is the
  correction, applied before the line feed since the terminal's scrollback is write-once.
* An attach seeds the terminal's scrollback from the worker's history before the first full frame
  (`Attached.Snapshot`, up to `[attach] scrollback_rows` = 1,000 rows).
* The counts come from libghostty's `ScrollbackRows`; at the scrollback's cap, where the count
  stops growing, the worker falls back to comparing the rows shifted (`scrolledRows`), which is
  the same heuristic this plan needs for the alternate screen.

## Where the framebuffer lives

The question was whether the client could keep a framebuffer and predict the rows that leave the
screen. It can, and it already has the rows: the painter's cache holds the top rows of the viewport
just before they go. But a client sees one screen per round trip and only while it is attached.
The worker sees every write.

| | Client (the painter) | Worker (new record) |
|---|---|---|
| Granularity | one diff per frame: on sfo (332 ms round trip) a dozen rows of streaming per diff | one diff per feed: a row or two, and the exact bytes of the row as it left |
| While nobody is attached | nothing | records, so a reattach seeds the whole transcript into the terminal's scrollback |
| Clients | each infers its own, differently | one record, every client gets the same rows |
| `agt history` | unchanged: the transcript of an alternate-screen program is lost at exit today | the record is written into the history |
| Protocol | none | one capability, one frame field |
| Pure Go | yes | the matcher is pure Go in `internal/screen`; the worker calls it |

So: the matcher that infers the scroll is a pure function over rows, shared; the client's
framebuffer keeps doing the painting; the record of rows that left the viewport is the worker's,
and reaches the client through the fields frames already have. A client-side prototype is still the
fastest way to find out whether the inference holds up against a real Claude Code (Phase 2).

## The model

The program's alternate screen is a viewport onto a transcript that agtd never sees whole. The
record reconstructs the transcript from what passed through the viewport:

```text
the transcript as rendered, logical rows numbered from the first row the viewport showed

  0                            mark                        pos           pos+R
  |·········· emitted ···········|······ not yet ·······|=== viewport (R rows) ===|
    in the terminal's own scrollback, write-once             the alternate screen, rows
                                                             [top, top+R) of the frame
```

* `R` is the viewport: the alternate screen minus fixed chrome. For Claude Code, Codex and pi
  that is the whole screen minus the rows at the bottom (input box and status rows), which the
  matcher tolerates by itself (below); fixed rows at the top would come from the program's
  profile (`top`), none known today.
* After each write to the alternate screen the worker formats the screen's rows (one formatter run,
  19 µs for 200x50, split at CRLF as frames do) and compares them with the rows after the previous
  write. The matcher returns a shift `k`:
  * `k > 0`: the content moved up by k rows. `pos += k`. The old rows that were at `[top, top+k)`
    left the viewport; those of them with logical index at or past `mark` are appended to the
    record and `mark = pos`. Rows below `mark` have been emitted before (the viewport came back
    down over rows it had already shown) and are not emitted again.
  * `k < 0`: the content moved down (the user scrolled the viewport up, or the input box shrank
    and the rows above it came back). `pos += k`, nothing is emitted.
  * no shift: rows changed in place (a spinner, a streaming tail, a reflow) or the screen changed
    wholesale (a jump: `/clear`, a transcript view, the viewport snapped to the end). In-place
    changes need nothing. A wholesale change resynchronizes `pos`: the new viewport's first rows
    with content are looked up in the record (every emitted row is there, hashed), and if found
    `pos` is that index, exact. If not found the rows are new: `pos = mark`, so what is on screen
    is emitted as it leaves and nothing is duplicated; whatever scrolled past between the two
    screens is a gap, counted.
* The record is a ring of VT rows bounded by count (`maxAltScrollbackRows`, 10,000 to start: about
  1 MiB at 100 bytes a row, the same order as the primary scrollback's 10 MB cap), with a
  monotonic count of rows ever emitted. Frames use the count exactly as they use
  `ScrollbackRows` on the primary screen: `Scrolled` is its growth since the client's last frame,
  `History` the last min(growth, 1,000) rows. No matching happens at the frame level.

"Near-native" names the three ways this differs from a terminal's own scrollback, all consequences
of the terminal's buffer being write-once: a row that the program rewrites after it left the top
(a reflow when scrolling back) stays as it was; a jump of more than a viewport between two writes
loses the rows in between; and a failed resynchronization prefers a gap to a duplicate.

## Phase 1 results (2026-10-23)

`claude-scroll.vt.gz` (Claude Code 2.1.308, 120x40, 121 KB) prints "shell row 001" to "shell row
120" from bash mode in four commands, scrolls with the wheel up, down, far up and far down, clicks
the transcript, types and deletes a three-line message, and clears. `TestTranscriptRecordsClaudeScroll`
replays it through the record at five granularities and counts the shell rows recorded:

| Looks at | Looks | Rows recorded | Jumps | Missing | Duplicated | In order |
|---|---|---|---|---|---|---|
| every repaint | 336 | 159 | 5 | 0 | 0 | yes |
| every 4 repaints | 84 | 146 | 5 | 0 | 0 | yes |
| every 16 repaints | 21 | 144 | 5 | 0 | 0 | yes |
| 2 KiB reads, and each block end in one | 337 | 159 | 5 | 0 | 0 | yes |
| 16 KiB reads, and each block end in one | 336 | 159 | 5 | 0 | 0 | yes |

The 39 rows besides the shell rows are the transcript as drawn: the banner, each command line,
Claude Code's reply to it, the turn summaries, `/clear` and its tip. One row per command is stale:
"⎿ Running…", the placeholder Claude Code drew and then rewrote in place with the first output row,
recorded because the output arrived as more than a screen at once (a jump) and the jump records
what the old screen showed. That is the write-once cost "near-native" admits.

What it took, each found on the fixture:

* **Rows are compared by a key, not their bytes.** The formatter writes an empty cell as a cursor
  move and a space as a space, and Claude Code's diff renderer leaves either behind depending on
  what was there before: "shell row 004" arrived as `ESC[5Cshell ESC[1Crow ESC[1C004` where
  "shell row 005" had spaces. With byte equality almost every row was missed. `RowKey` turns
  cursor moves into spaces and drops trailing spaces and pen resets; the recorded rows keep their
  bytes.
* **Looks happen outside synchronized output only.** The bindings expose the mode
  (`libghostty.ModeSyncOutput`), so a look is skipped while a block is open and taken at each block
  end. Reads cut at arbitrary points, looked at mid-repaint, duplicated 30 rows and lost 25.
* **A shift needs a run of at least four rows with two that occur once.** Claude Code's screen
  after `/clear` matched its last screen at a shift of one, two rows long. Real shifts match 13
  rows or more.
* **A jump back is found only if the whole new screen matches.** The banner after `/clear` matched
  the banner recorded at the start, three rows, which made the record skip the rows still on
  screen. The lookup now needs the rows above the input box to match until the record or the
  transcript ends, or for at least half of them (Claude Code draws "Jump to bottom" over the last
  row while scrolled).
* **The transcript ends above the input box.** Without it, each jump recorded Claude Code's box:
  the rules, the `❯` and its hint row. The caller passes how many rows from the top are
  transcript; the worker knows it from field detection (`Field.BoxTop`, less the hint row above
  it). A jump also records at most one trailing blank row.

Answered on the fixture: open question 2 (the thresholds above; no fixed rows at the top in Claude
Code), 5 (yes, `ModeSyncOutput`), and part of 9 (the scrolled transcript is interactive: Claude
Code draws a "Jump to bottom (click) ↓" target over its last row; the click on a transcript row
did nothing visible). The wheel scrolls three rows a tick. Open questions 1, 3 and 6 need the
streaming fixture, `claude-scroll-stream`, which is scripted but not recorded: Claude Code was
logged out on the recording machine.

## Decisions

1. **The worker records, per feed, on the owner goroutine.** After `afterFeed` in `publishScanned`
   (`internal/worker/owner.go`), when the active screen is the alternate one and the program is
   profiled (decision 4): format the rows, run the matcher against the previous rows, update
   `pos`, `mark` and the ring, keep the rows for the next diff. The pump already coalesces reads,
   so a diff costs one formatter run per read; at a 1.4 MiB/s flood in 4 KB reads that is 350
   runs a second, about 7 ms of CPU a second. No floor to start; `agtd bench sessions` says
   whether one is needed. If the libghostty bindings expose synchronized output (mode 2026), the
   diff waits for the block to end, capped at 150 ms, so a repaint split across two reads is seen
   whole; a half-seen repaint is handled correctly anyway (the shifted top rows match at `k`, the
   rest at the next read matches at 0), this only saves runs. Phase 1 showed it is not optional:
   looks inside a block duplicate and lose rows, so the diff runs at every block end within a read
   and is skipped while a block is open.

2. **The matcher is a pure function in `internal/screen`**, over rows as the formatter writes
   them, compared by `RowKey` (Phase 1 results: equal cells can come in different bytes).
   For each shift `k` in `±(R−1)` it finds the longest run of consecutive rows with
   `new[i] == old[i+k]`. A shifted run wins over "nothing moved" (the run at `k = 0`) when it is
   longer; the rows that stay put under a real scroll are the chrome at the bottom and a changing
   tail, the rows that stay put under a spinner tick are the whole rest of the screen. A run only
   counts if it holds at least two rows with content and at least one row that occurs once in the
   old screen, since Claude Code's `─` rules, blank separators and repeated list bullets match at
   many shifts. Ties go to the smaller `|k|`. Settled on the fixture: at least four rows, two of
   them unique. The rows that left are `old[top : top+k]`.

3. **Position, mark and resynchronization** as in the model. The ring keeps a map from a row's
   hash to its logical index for the lookup; the lookup uses the first three rows with content of
   the new screen and requires them consecutive in the record. This covers the common jump, the
   user scrolling far back with the wheel or PageUp and later returning, without duplicates. Rows
   that reflow between being emitted and being looked up defeat it, and the fallback is
   `pos = mark`.

4. **Only programs known to draw a transcript on the alternate screen are recorded.** The field
   profiles (`fieldProfiles` in `internal/worker/field.go`, keyed by the OSC 7501 app, then the
   agent, then the command) gain `altScrollback bool` (and `top int` for fixed rows), set for
   `claude-code`, `claude`, `codex` and `pi`. A shell session that runs vim, htop or less records
   nothing, and its client mirrors the alternate screen exactly as today; pushing an editor's
   buffer into the user's scrollback as it scrolls would be noise. The decision is per program,
   not per session: a shell session in which the user runs `claude` records while Claude Code is
   up, and each frame says whether it does (protocol below), so the client switches at the full
   frame that every screen switch already produces.

5. **The client stays on the primary screen** while a frame says the alternate screen is being
   recorded: the painter drops `?1049`, `?47` and `?1047` from the prefix (it never enters the
   alternate screen), paints the viewport's rows on the primary screen, and pushes `History`
   with line feeds exactly as for the primary screen; the painter's scroll path does not change.
   The seams:
   * *Into the alternate screen.* The client's primary rows are overwritten by the full frame,
     not scrolled: the `$ claude` line is under the transcript, not above it. Pushing the primary
     screen's rows down to the cursor into the scrollback first would put them above, but they
     come back when the program exits (the full primary frame repaints them) and would then show
     twice at the seam. Not pushed; open question 7.
   * *Back to the primary screen.* The full frame repaints the restored primary screen; the
     transcript stays in the scrollback above it, which is better than the local terminal, where
     leaving the alternate screen discards it.
   * *Detach and exit.* `attachRestoreSequence` writes `?1049l`, a no-op on a terminal that never
     entered it. The viewport stays on screen like the output of a primary-screen program would,
     so the restore moves the cursor to the last row and writes a newline, and the shell prompt
     starts under the viewport instead of over it. Today detach returns to the screen the user
     had before attaching; in this mode it cannot, and should not.
   * *Repaints.* The overlay closing and a reattach repaint through `attachRepaintSequence`, which
     clears the screen; some terminals push a cleared screen into the scrollback (Terminal.app
     does, iTerm2 by option) and would duplicate the viewport each time. In this mode a repaint
     is a full frame painted row by row with no clear, which the painter already does for a full
     frame.
   * *Resize.* A full frame, as today. Rows already pushed keep the width they were formatted at;
     the terminal reflows its scrollback or not, as it does for the primary screen's rows now.

6. **Attach seeds the transcript.** Under the alternate screen `Attached.Snapshot` holds the
   primary screen's scrollback today, formatted from a decoded copy of the terminal
   (`historySnapshot`, `primaryUnderAlternate`). It gains the record's rows after them, within the
   same `ScrollbackRows` budget, the record first: the newest rows matter most, so the record's
   last rows are taken, then the primary scrollback fills what is left. Then the full alternate
   frame. A reattach after hours shows the whole run in the scrollback and the live viewport under
   it.

7. **`agt history` includes the record.** The history formatted at exit (`terminalState.format`)
   is scrollback then screen; it becomes scrollback, record, screen, which is the order things
   happened in. `history.vt` likewise.

8. **The record survives a handoff.** `handoffState` gains the ring, its counts, `pos` and `mark`
   ("nil from an older image", as the other fields). A worker crash loses it along with the rest
   of the terminal; it is not persisted in `state.db`.

9. **Mouse reporting is left as the program set it.** With Claude Code's mouse modes on, the wheel
   scrolls Claude Code's viewport and the terminal's scrollback is reached the way every terminal
   provides for mouse-mode programs: shift-wheel in Ghostty, kitty and WezTerm, the scrollbar or
   keys elsewhere (open question 4 lists what to verify). A later overlay item, "Release mouse",
   can filter the mouse modes locally so the wheel scrolls natively; it is not needed to try this.

10. **Frames only.** The raw attach passes the program's bytes through, so it mirrors the
    alternate screen and cannot do this. A local attach gets it with `[attach] mode = "frames"`.
    Whether frames should become the local default is a separate decision this feature adds a
    reason to; BENCHMARKS.md "Screen frames against raw output" has the local numbers.

11. **The client keeps a cursor, and a bounded copy for prediction.** The terminal's own
    scrollback is the client-side record that the user reads, and it is write-once: no terminal
    reports where the user has scrolled to, and rows can only ever be added at the bottom, so
    nothing pushed there is ever corrected. The client keeps the count of record rows it has
    pushed. A reattach asks for the rows since that count (`AttachSession.SinceRow`, below)
    instead of the zero rows it asks for today (`reattach` in `internal/cli/attach.go`, "the
    terminal already holds the scrollback the first attach seeded"), so rows that scrolled off
    during an outage reach the terminal once, after the reconnect, and nothing is pushed twice.
    The same gap exists for the primary screen's history today; closing it the same way needs a
    count that does not fall at the scrollback's cap, so it is a follow-up, not part of this plan.

    Revised 2026-10-25: the client also keeps the last `altPredictRows` (2000) record rows in
    memory, under a megabyte, as the source for decision 12's prediction. It is a cache for
    drawing a guess, never a second record: the terminal's scrollback and the worker's ring stay
    the records.

12. **Wheel scrolling of the agent's transcript is predicted locally, and the frame reconciles
    it.** Revised 2026-10-25, after a day on sfo (199 ms away): with the agent's viewport a round
    trip away, the plain wheel felt nothing like a local Claude Code, and the user's goal is a
    local framebuffer that answers at once while the worker's frames stay the authority. The
    first revision rejected this; the record the prototype builds removes its main obstacle, since
    the client now holds the rows above the viewport. Three ways to render the record were
    weighed:

    | | Terminal-native | A local view by agt | Predict the agent, reconcile (now chosen for the wheel) |
    |---|---|---|---|
    | Latency | none, with the terminal's own smooth scrolling | one write of a screen per tick | none until a correction |
    | Correctness | exact, nothing to replace | exact, nothing to replace | a guess per agent and release, corrections visible as the text jumping |
    | Search, selection | the terminal's | agt's to build | the agent's, a round trip away |
    | After detach | the transcript stays in the terminal | gone | gone |
    | Costs | the seed before the first frame, write-once rows, the wheel taken from the agent | chrome of agt's own, a scroll position the agent does not know about | all of the above plus a confidence model like mosh's |

    The three are not exclusive. Native scrollback stays (shift-wheel, decision 9) and keeps the
    whole transcript; prediction covers the plain wheel, which the agent owns. How it works:

    * **The guess.** A wheel report inside the transcript region (above `Field.BoxTop`) is sent
      to the agent as today, and the painter at once shifts the transcript rows by the expected
      step, filling the uncovered rows from the copy (down) or from the rows the agent last
      showed (up). The input box and the rows below it are left alone. Nothing is guessed for a
      wheel outside the region, for clicks, or for keys.
    * **The step.** Claude Code's rows per tick is learned, not assumed: the shift matcher
      (`Shift`) measures each authoritative frame that follows a tick against the frame before
      it, and the client keeps the last measured step (3 until measured). Several ticks before a
      frame arrives add up.
    * **Reconcile.** The worker's frames stay the screen's authority. Each frame is painted as it
      is today; the guess is only what the terminal shows between a tick and the frame that
      answers it. A frame built before the tick reached the worker (its `Acked` is below the
      ack sent after the tick, decision 13) does not answer it, so the guess is re-applied over
      it instead of jumping back. A frame that answers it and differs is a correction: the guess
      was wrong and the step is relearned from it.
    * **Confidence.** After two corrections in a row, guessing stops until a guess would have
      been right twice (the echo's pause, `internal/cli/echo.go`, has the same shape). It also
      stops while the frame does not look like the transcript (no `Field`, a dialog open), at the
      top of the copy, and inside a synchronized-output block.

    What a wrong guess costs is one visible jump to the agent's real viewport, a round trip after
    the tick. The open risk is how Claude Code draws a scrolled-up view (a "new messages" marker,
    rewrapping); the `claude-scroll` fixture measures it: every predicted screen is compared with
    the frame that answers it.

    Built 2026-10-25 (`internal/screen/predict.go`, `[attach] predict_wheel`, on under
    `alt_scrollback`). Over the fixture's 79 wheel ticks, one repaint each
    (`TestFramesPredictWheelClaudeScroll`): 78 guessed, 1 taken back (Claude Code moved two rows
    for one tick), and 58 of about 2,650 rows drawn corrected by the frame that answered, all at
    the top edge; four ticks to a frame: 75 guessed, 1 taken back, 37 rows. Claude Code moved one
    row per tick. What the fixture showed about its scrolled view:

    * It draws a "Jump to bottom (click) ↓" over the transcript's last row while scrolled up. The
      guess learns it as chrome (a row moved from the agent's screen that came out wrong in two
      guesses in a row is pinned) and leaves it as drawn.
    * Its view scrolled back is not the transcript as it streamed: a finished tool's output is
      collapsed ("… +27 lines"), and the top row is often left blank. So the rows the record holds
      are a different rendering from the one scrolled back to, and the guess fills a row from the
      rows as last seen in the agent's own view first (`seen`, a bounded copy of 2,000 rows each
      side, decision 11), the record only for rows not seen yet. The top row or two of a tick into
      unseen territory are the corrections above.

    The prediction does not need the terminal's scrollback, only the record (2026-10-26). With
    `alt_scrollback` off and `predict_wheel` on, the client mirrors the alternate screen 1:1, as
    Claude Code's fullscreen renderer draws it (the composer fixed at the bottom, no terminal
    scrollback), and the painter keeps the record for its guesses alone; the `mirrored` case of
    `TestFramesPredictWheelClaudeScroll` guesses as the others do and leaves the client terminal
    without scrollback. The two then answer different wishes: `alt_scrollback` for the terminal's
    own search, selection and history after detach; the mirror for the agent's own fullscreen
    view at local speed, where the terminal's scrollback is a second view the agent does not
    know about (a keystroke jumps it back, and an overlay cannot be drawn over it).

    Live on sfo3 (2026-10-26, Claude Code 2.1.310, ~195 ms away), three habits the fixture's
    synthetic ticks had hidden:

    * Claude Code does not move for the first tick after the wheel turns around; the second
      moves one row. The guess leaves that tick undrawn too, and learns otherwise if an agent is
      seen to move for it (the replay now sends the extra tick at each reversal).
    * A client attached to a long transcript has nothing above the screen in its record, so the
      first pass up draws nothing and goes at the round trip; rows the agent has shown since are
      guessed. Ticks at that edge are no evidence against the guessing (they had paused it after
      two). Seeding the record at attach is Phase 3's worker record.
    * A row under the agent's pinned chrome ("Jump to bottom") is unknown until the agent draws
      it; the guess leaves it as it is for the frame to correct rather than giving up the guess.

    Fast bursts accelerate (six quick ticks moved eight rows); the learned step absorbs part of
    it and the frames correct the rest.

    A local view of agt's own (tmux's copy mode) stays rejected: a viewport the agent does not know
    about, with search and selection to build, against the rule that agt draws nothing the agent
    did not draw. The guess draws only what the agent is about to draw.

13. **The frame window follows the round trip.** At most two frames were in flight
    (`protocol.FrameWindow`), so a link with round trip R got at most 2/R frames a second: 10 on
    sfo, and a streaming answer advanced in visible steps. A client that asks for
    `attach-frame-window` gets a window the worker sizes from the round trip it measures between
    sending a frame and reading its ack: `ceil(minRTT / 16ms) + 1`, between 2 and 16, so frames
    can come about every 16 ms on any link. It uses the minimum round trip of the last 10 s, not
    the smoothed one: queueing delay raises the smoothed one, and a window grown from it would
    queue more (bufferbloat feeding itself). Each frame then carries `Acked`, the last ack the
    worker had read when it built the frame, since the local echo can no longer derive it as
    `Seq - FrameWindow`; the frame shows the input the client sent before that ack. Against an
    older worker or client the window stays 2.

## Protocol additions (under one capability; golden frames and fuzz for it)

* `attach-alt-scrollback` (`CapAttachAltScrollback`), an attach-stream capability listed in
  `AttachSession.Features` like `attach-frames`, which it requires.
* `Frame.AltScrollback bool` (encoded only under the capability): this alternate screen is being
  recorded, so `Scrolled` and `History` may be non-zero on an alternate frame and the client should
  stay on the primary screen. It is false on primary-screen frames and on alternate screens of
  unprofiled programs, where everything is as before. A change of its value always comes on a
  full frame, since it only changes with the program or the screen.
* `AttachSession.SinceRow uint64` (encoded only under the capability): seed `Attached.Snapshot`
  with the record's rows from this index on, within `ScrollbackRows`, instead of its last rows. A
  first attach sends 0 and gets the last rows as usual; a reattach sends the count it has pushed.
  The worker clamps to what the ring still holds.
* No new response or `FrameAck` field. `Attached.Snapshot` is bytes and its content changes
  without a protocol change. A worker without the capability sends what it sends today
  and the client mirrors the alternate screen, as today.

## Bounds, goroutines and failure

* No new goroutine. The diff and the ring live on the owner goroutine; the frame handler reads the
  ring's count and rows under the same `doAtGround` boundary it builds frames at.
* Memory per session: the ring, at most `maxAltScrollbackRows` rows of VT plus a hash index, about
  1 MiB at the default; one screen of previous rows for the diff. Nothing per attachment beyond
  what frames keep.
* CPU: one formatter run per feed on the alternate screen (19 µs for 200x50, scaling with rows)
  and an `O(R²)` row comparison with early exits; measured in `agtd bench sessions` with an
  alternate-screen flood before the default is set.
* Loss is bounded and counted: a jump of more than a viewport between two diffs loses the rows in
  between (`gaps` and `gapRows` in `GetSessionStats`, shown by `agt status --stats`), a frame
  carries at most 1,000 rows of history and a full frame after more leaves the rest blank, as on
  the primary screen. The ring drops its oldest rows at the cap.
* Nothing here can stall the PTY or another client: the record is built from the screen, not from
  the output stream, and the frame pacing (two in flight, acknowledged) is unchanged.

## Configuration

```toml
[attach]
# Under frames: rows that leave the top of an agent's full-screen transcript
# (Claude Code, Codex, pi) go into this terminal's own scrollback, and the
# terminal stays on the primary screen while the agent runs. Off mirrors
# the alternate screen, as a raw attach does.
alt_scrollback = true
# Under frames, the wheel over the transcript of an agent on the alternate
# screen scrolls it at once, with or without alt_scrollback (decision 12).
#predict_wheel = true
```

The worker's `maxAltScrollbackRows` starts as a constant beside `maxScrollbackBytes`; a daemon
setting follows only if a real session needs more than 10,000 rows.

## Testing

* **Fixture first.** Record `claude-scroll.vt.gz` with marks, using the recorder in
  `internal/worker/testdata` as the field fixtures were: Claude Code at 120x40. The transcript is
  filled without the model, with `! seq` shell commands whose output Claude Code shows in the
  transcript (`! seq 1 20`, `! seq 21 40`, and so on until the viewport has overflowed several
  times), so every row is unique, ordered and known; then the wheel up and back down (SGR mouse
  reports written to the PTY, `CSI < 64 ; x ; y M` and `65`, since Claude Code enabled mode
  1006), a three-line typed message so the box grows and shrinks, `/clear`, exit. One prompt to
  the model (an answer of 200 numbered lines) is the only way to record a streaming tail shifting
  under the matcher, so it is a second scenario, recorded only with the user's say-so, since it
  uses their account. The same later for Codex and pi full-screen. The marks say what the
  transcript was at each step, so the test can count missing and duplicated rows.
* **The matcher** (`internal/screen`, table tests): shift up and down, box growth and shrink,
  changing bottom rows, a changing tail during a shift, repeated rows and rules, blank screens, a
  half-applied repaint, a wholesale change with and without a findable position, resize.
* **The worker** through the real harness (`TestFrameClientThroughTheWorker` is the model) with a
  scripted transcript program: a shell script that enters the alternate screen and repaints
  numbered rows by absolute position, shifting them up one at a time, so no agent is needed and
  the expected scrollback is exact. Frames carry `Scrolled` and `History`, the replayed client
  terminal (`TestFramesReplayStreams` style, a libghostty terminal with scrollback) holds rows
  1..N once and in order, the attach seeds the record, the switch back to the primary screen is a
  full frame with `AltScrollback` false, a handoff keeps the ring, a kill mid-record leaves no
  goroutine.
* **The fixture replayed** through worker and painter into a second terminal: rows missing and
  duplicated against the marks. Acceptance: zero duplicates; gaps only across the jumps the fixture
  contains deliberately, with their rows counted.
* **The CLI**: the prefix without `?1049h`, the restore leaving the viewport and a newline, a
  repaint without a clear, the `alt_scrollback = false` path mirroring as today; golden frames and
  fuzz for the new field; `cmd/agt` end to end with the scripted program.

## Order of work

1. **Fixture and matcher** (1-2 days): record the Claude Code fixture; write the matcher and its
   tests; run it over the fixture frame by frame and per write, and report missing and duplicated
   rows for each. This settles open questions 1-3 and whether per-frame inference (the client
   prototype) is good enough to be worth a day of hands-on use.
2. **Client prototype** (1-2 days, no protocol change): under `[attach] alt_scrollback`, the CLI
   infers the scroll from consecutive alternate frames with the matcher and feeds the painter a
   synthetic `Scrolled`/`History`; the painter stops mirroring the alternate screen; the detach
   and repaint seams. Built 2026-10-23: the painter does it (`SetAltScrollback`), the CLI asks
   for the field in its acks so the record stops above the input box, and detach leaves the
   cursor under the screen. Not done: the repaint without a clear (decision 5), the reattach cursor
   (decision 11), and limiting it to profiled programs (decision 4), which the client cannot tell
   without the worker. Use it on sfo with a real Claude Code for a day. Decide whether to go on.
3. **The worker record** (about a week): the ring, the per-feed diff, the profile flag, the
   capability and `Frame.AltScrollback`, attach seeding, history, handoff, stats, the bench. The
   client takes the worker's counts when the capability is offered and drops its own inference;
   against an older worker it mirrors the alternate screen as today.
3a. **Frame window and wheel prediction** (decisions 12 and 13, revised 2026-10-25): the
   `attach-frame-window` capability, `Frame.Acked` and the worker's RTT-sized window first, then
   the client's prediction of the plain wheel, measured over the `claude-scroll` fixture.
4. **Finishing**: the "Release mouse" overlay item if the first days of use want it; the local
   frames default decision; BENCHMARKS.md numbers; ARCHITECTURE.md "Screen frames" gains a
   subsection, README and ROADMAP a line.

## Open questions, settled by the fixture before Phase 3

1. What Claude Code does when new output arrives while the viewport is scrolled up: snap to the
   end (a wholesale change, resynchronized by lookup) or stay (shifts continue). Decides how much
   the lookup matters.
2. The matcher's thresholds: minimum run, content rows, the uniqueness rule, and whether `top`
   chrome exists in Codex or pi full-screen.
3. Whether Claude Code rewrites rows that already left the top when the user scrolls back to them
   (reflow), and how often: the staleness "near-native" admits.
4. Terminal behaviour to verify, per terminal (Ghostty, kitty, WezTerm, iTerm2, Terminal.app,
   Alacritty): does shift-wheel reach the scrollback with mouse reporting on; does `CSI 2 J` push
   the screen into the scrollback; does the scrollback reflow on resize.
5. Whether the libghostty bindings expose mode 2026 state to diff at the end of a synchronized
   block, and what cap to use when a program leaves it set.
6. Whether a program's own transcript view (Claude Code's full transcript mode) rewrites the same
   rows identically, so the lookup finds them, or differently, so they would be emitted again.
7. Whether to push the primary screen's rows down to the cursor into the scrollback when the
   program enters the alternate screen (the `$ claude` line above the transcript) and accept the
   duplicate at exit, or not (this plan).
8. Names: `attach-alt-scrollback`, `Frame.AltScrollback`, `[attach] alt_scrollback`.
9. Whether Claude Code's scrolled transcript is interactive. It enables any-event mouse tracking
   (mode 1003), which hints at hover or click targets. If reading is all the viewport is scrolled
   for, the wheel can go to the terminal by default for profiled programs (decision 9's toggle
   becomes the default) and native scrollback is reached with the plain wheel; if the transcript
   has click targets, the toggle stays the compromise. The fixture records clicks on a message to
   find out.

## Explicitly not in this plan

* Reading the agents' own transcripts (Claude Code's JSONL, Codex's logs). The record is what the
  user saw rendered, for any program, and stays agent-agnostic.
* A scrollable viewport of agt's own, a pager, or any chrome the agent did not draw. The
  terminal's scrollback is the viewer.
* Scrollback for arbitrary alternate-screen programs (editors, pagers, monitors).
* Reflowing pushed rows on resize, or correcting rows already in the terminal's scrollback.
* Persisting the record in `state.db`, or an artifact stream for it (`agt history` covers exit;
  a live `transcript` artifact can come later if wanted).
* Changes to the raw attach.
