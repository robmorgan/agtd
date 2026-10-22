# TODOS

## Product

### Monitor "multiplexer" terminology confusion

**What:** Track whether the term "agent multiplexer" causes users to expect
tmux-like pane management.
**Why:** Design doc Open Question #1 — the word sets expectations agtd
deliberately doesn't meet.
**When to act:** If 2+ of first 5 onboarded users express confusion.
**Action:** Consider reframing to "agent supervisor" or "agent runtime".
**Context:** CEO plan accepted "agent multiplexer" positioning. The README
section draws the boundary, but terminology may still mislead.
**Priority:** P3

## Attach

### Terminal queries are dropped while the local echo holds output

**What:** While `agt attach` holds output for unconfirmed predicted keys
(internal/cli/attach.go, handleFrame), forward the terminal queries in it, or
have the worker answer them as it does OSC 10/11.
**Why:** A query the agent writes then (cursor position `CSI 6 n`, device
attributes, kitty `CSI ? u`, DECRQM) never reaches the terminal, so its reply
never comes; a program that waits for one (crossterm reads the cursor
position) stalls until its timeout.
**Context:** Found reviewing the native echo (PR #59) fixes. The echo itself
forwards every reply it sees unchanged; the gap is that held output is dropped
whole and the snapshot repaint does not replay queries. Options: scan dropped
output for query sequences and write those, or answer CPR/DA in the worker
(internal/worker/colors.go is the model).
**Effort:** M
**Priority:** P2
**Depends on:** None

### Drop key releases whose press agt consumed

**What:** Keep `agt attach` from forwarding the kitty release event of a key
the CLI swallowed itself: the hotkeys (Ctrl-Y, Ctrl-\, Ctrl-[, Ctrl-]) and the
keys that close the overlay.

**Why:** Agents that ask for release events (pi pushes kitty flag 2) get a
release with no press; after Ctrl-] the release lands on the next session's
agent. Agents usually ignore an orphan release, but one that tracks modifier
state could see a phantom Ctrl.

**Context:** `parseAttachHotkeyCSIu` (internal/cli/input.go) refuses event
type 3, so a hotkey's release passes through as data. This has always been so
without local echo; with echo, releases were dropped by the echo's decoder
until echo-mouse-passthrough made the echo forward them (red-team finding,
deferred there). Hotkey releases are a small change in the parser; keys the
overlay consumes need their presses remembered across the overlay closing and
a session switch.

**Effort:** S
**Priority:** P3
**Depends on:** None

### Decide which modes a repaint resets

**What:** Extend `attachRepaintSequence` (internal/cli/attach.go) beyond mouse
reporting to the other input-shaping modes a snapshot only ever turns on:
bracketed paste (?2004), application cursor keys (?1), autowrap (?7).

**Why:** A snapshot emits only modes that differ from the defaults. A mode the
agent turned off while local echo held its output, or during a reconnect,
stays on in the client terminal: pastes arrive wrapped, arrows in SS3 form.

**Context:** echo-mouse-passthrough added the mouse reset and found that
resetting focus events (1004) made Ghostty report the focus on re-enable, a
phantom focus-in; so check each mode's re-enable side effects in Ghostty and
xterm before adding it, with a test per mode. Focus reporting itself can stay
on after the agent turned it off during a disconnect (the echo now drops
repeated focus reports, but a real change would still reach an agent that no
longer asked for them); resetting it needs the echo to swallow the report the
re-enable provokes, which the repeat filter already does.

**Effort:** S
**Priority:** P3
**Depends on:** None

### Pass OSC and DCS query replies through the echo without pausing

**What:** Decode `ESC ]`, `ESC P` and `ESC _` strings up to BEL or `ESC \` as
one keyPassive event in `decodeKey` (internal/cli/input.go), holding an
unfinished one across reads.

**Why:** They decode as an Alt key plus characters today, so a late reply
(the CLI's own OSC 10/11 colour query answered after its timeout, an agent's
XTVERSION or DECRQSS) pauses local echo for a round trip. The bytes still
reach the agent unchanged.

**Context:** CSI replies (cursor position, device attributes, mode reports)
became passive in echo-mouse-passthrough; this is the remaining form. Related:
replies to the CLI's own colour probe (colors.go: OSC 10/11 then DA1, 300 ms)
that arrive after it gives up, on a slow terminal, are forwarded to the agent
as input, echo on or off; consume those for a short window after the probe
times out.

**Effort:** S
**Priority:** P3
**Depends on:** None

### Classify passive reports split across input frames

**What:** Keep `isPassiveInput` (internal/worker/terminal.go) state per
attachment so a mouse or focus report split across two `AttachInput` frames is
still recognised as passive.

**Why:** Without local echo, stdin reads go to the worker as they arrive; a
wheel report cut in two counts as typing and answers a blocked record. It errs
the safe way for input and matches how all input counted before
echo-mouse-passthrough, but a scroll can still hide a permission prompt.

**Effort:** S
**Priority:** P3
**Depends on:** None

### Stamp frames with the input they show

**What:** Have each `AttachFrame` carry how much of the attachment's input
the worker had read when it built the frame (an input sequence number or
byte count), and have the local echo confirm keys against that instead of
deriving it from the frame window (`frameField` in internal/cli/echo.go).

**Why:** Today the echo proves what a frame shows from ordering: the worker
reads input and `FrameAck`s in order on one stream and builds frame N only
after the ack of frame N-`protocol.FrameWindow`. That holds over a QUIC
stream, but breaks if frames or acks move to unreliable QUIC datagrams
(mosh-style, a lost frame replaced by the next instead of retransmitted).
A stamp states it outright: no ignored "unproven" fields and no re-probing
to get past the window, so keys confirm a frame or two sooner.

**Context:** Raised merging attach-frames (#66) into echo-review (#68). It
is a new field in `Frame`, so a capability and golden-frame updates; keep
the window rule for workers without it. Only needed before frames go
unreliable; input itself stays on a reliable ordered stream.

**Effort:** M
**Priority:** P3
**Depends on:** None

## Completed

### Keep the echo paused when input pauses it during a field probe

**What:** Have `fieldAnswer` stay paused and ask again when an unpredicted key
or a click arrived after the field was asked for.

**Completed:** echo-review (2026-10-22): a pause or resize makes the
outstanding field stale (`epoch` in internal/cli/echo.go).
