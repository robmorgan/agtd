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

### Keep the alt scrollback prototype's rows across the overlay, detach and exit

**What:** With `[attach] alt_scrollback = true`, three paths lose or damage
what the terminal's scrollback should hold:
- Rows the agent scrolls off while the Ctrl-Y overlay is open become
  `History` in the painter, but the overlay owns the alternate screen and the
  paint is thrown away; closing it restores a full frame that the record has
  already seen, so those rows never reach the scrollback.
- Detach and switch write `?1049l` and clear the screen, which erases the last
  screen from the primary screen the prototype paints on.
- When the session exits, the closing repaint overwrites the last screen.
**Why:** The prototype's promise is that the terminal's scrollback holds the
whole transcript; each path leaves a gap or an erased screen in it.
**Context:** Found by the PR #81 pre-landing review and deferred to Phase 3
(docs/designs/alt-screen-scrollback.md), since the mirror mode
(`alt_scrollback = false`) is the default and the worker record changes how
the record is seeded. For the overlay: keep the History produced while it is
open and write it after `ov.leave()`, before the restored frame.
**Effort:** M
**Priority:** P2
**Depends on:** Phase 3 (the worker record)

### Measure the per-frame costs of the record and the overlay backdrop

**What:** `RowKey` is computed for every row of every alternate-screen frame,
and the overlay repaints the whole backdrop on every frame while it is open.
**Why:** Both are linear in the screen per frame; neither has been measured
against a fast-scrolling agent on a large terminal.
**Context:** Noted by the PR #81 performance review. Cache keys for rows the
frame did not change; repaint only the backdrop rows a frame changed.
**Effort:** S
**Priority:** P3
**Depends on:** A benchmark showing either matters

## SSH Agent Forwarding

### Tie each forwarder to the attachment that opened it

**What:** Put a per-attachment token in `Attached`, which `ForwardSSHAgent`
must send back, so only the client holding an attachment can forward for it.
Have agt report a forwarder it was refused instead of dropping it.
**Why:** Any client that can reach the daemon can register as the forwarder
of another client's attachment before that client's own forwarder opens.
So can a local process talking to the worker's socket, which can also set
`Client`. Either can then see and answer the session's agent requests.
**Context:** From the security review of PR #82. All authorized clients are
trusted alike today, so this belongs with per-client permissions (ROADMAP,
Next). It is a protocol change: a field in `Attached` and `ForwardSSHAgent`,
a capability, and golden frames.
**Effort:** M
**Priority:** P2
**Depends on:** Per-client permissions

### Keep a session's forwarder after switching away

**What:** While an agt process and its connection live, keep the forwarders
of sessions it switched away from (submit and switch, the inbox, Ctrl-]).
When a session's size owner has no forwarder, route its agent requests to
the most recently active of those.
**Why:** Submit and switch hands a session work and moves on. That work
often ends in `git commit` or `git push`, which then finds no keys, because
forwarding follows the size owner.
**Context:** From the red-team review of PR #82. This changes design
decision 3 (the size owner's forwarder serves), so decide before building.
**Effort:** M
**Priority:** P2
**Depends on:** None

### Record relayed signatures on the laptop

**What:** Have agt keep its own record of each signature it relays: the
time, host, session, key and purpose.
**Why:** Today the record lives only in the devbox's `state.db`, on the
side whose processes the policy guards against. Any process running as the
daemon's user can edit or flood it.
**Context:** From the red-team review of PR #82. Phase 2's asking flow is
the natural place for it.
**Effort:** S
**Priority:** P3
**Depends on:** None

### Say when agt does not forward

**What:** When `[ssh_agent] forward` is set but agt does not forward, say
so once, before raw mode or in the overlay. The causes are no agent at the
socket, no key that `[ssh_agent] keys` allows, or a session that cannot
forward.
**Why:** Skipping a dead or keyless agent keeps the devbox's own agent
usable, but the user cannot tell why their keys are missing.
**Context:** A follow-up to the review fix in which agt probes its agent
before forwarding. A mistake in `[ssh_agent]` itself is already reported
this way (sshAgentWarning: around an attach, and in the inbox until a key).
**Effort:** S
**Priority:** P3
**Depends on:** None

### Rename the worker's SSH agent state

**What:** Rename `rt.agent` to `rt.sshAgent`, and the `agent*` fields and
methods of ownerState to `sshAgent*`, as handoffState's `SSHAgent*` fields
already read.
**Why:** In the same structs, "agent" already means the coding agent
(`agentPID`, `agentExited`).
**Context:** From the maintainability review of PR #82.
**Effort:** S
**Priority:** P3
**Depends on:** None

### Parse SSH agent messages with ssh.Marshal

**What:** Replace internal/sshagent's hand-rolled wire reader with
x/crypto/ssh's `Marshal` and `Unmarshal` over tagged structs.
**Why:** About 50 fewer lines.
**Context:** From the simplification review of PR #82. `FuzzParse` guards
the parser.
**Effort:** S
**Priority:** P3
**Depends on:** None

### Remaining SSH agent tests

**What:** Cover the cells the review left untested:
- a replacement owner whose reservation expires, or the socket stopping,
  while a connection waits
- an owner that never forwarded leaving, being cut, being replaced, or
  ending at a handoff
- an attachment ending with a request in flight
- a session without a socket whose exec fails
- agt leaving no agent connection behind when the session ends
- agt counting closing connections against its bound
**Context:** From the state-machines and testing reviews of PR #82; the
review output has a stub for each.
**Effort:** M
**Priority:** P3
**Depends on:** None

### Tell the worker when agt will not forward

**What:** When a reattached size owner's agt decides not to lend its agent
(no key `keys` allows, the agent locked or gone), say so to the worker, so
new agent connections stop waiting for that owner's forwarder.
**Why:** The worker waits up to 10 s for the forwarder of an owner that
forwarded before (after a cut, or while one is reopened). agt opens none
when its agent has nothing to lend, so every program in that window waits
out the 10 s before falling back.
**Context:** From the adversarial review of the PR #82 fixes. It needs a
frame, or a field in an existing one, so a capability.
**Effort:** S
**Priority:** P3
**Depends on:** None

### Notice keys leaving the local agent

**What:** Have agt look again at its agent's keys while it forwards, and
end the forwarder once none that `keys` allows is left, opening it again
when one comes back.
**Why:** agt checks for a key only when it opens a forwarder. A key added
for a while (`ssh-add -t`, `AddKeysToAgent 1h`) or removed later leaves a
forwarder that answers every request with no keys, hiding the devbox's own
agent until the next reattach.
**Context:** From the adversarial review of the PR #82 fixes. An answer
that lists no allowed key is a cheap trigger to look again.
**Effort:** S
**Priority:** P3
**Depends on:** None

### Agent sockets reached from inside a session

**What:** Decide what agt does when its own `SSH_AUTH_SOCK` is a session's
socket (agt run inside a session), and refuse to lend a session its own
socket.
**Why:** Lending another session's socket chains forwarding, as `ssh -A`
from inside a forwarded session does, and may be wanted. Attaching a
session to itself from inside it makes each agent request loop back until
both 64-connection bounds fill.
**Context:** From the adversarial review of the PR #82 fixes; the worker
side (an inherited `SSH_AUTH_SOCK`) is handled by `inheritedAgent` and `resolveAgentSocket`.
**Effort:** S
**Priority:** P3
**Depends on:** None

### Allocate agent messages as they arrive

**What:** Have `sshagent.ReadMessage` grow its buffer as the body arrives,
rather than allocating the length the header claims.
**Why:** A program in the session can send a header claiming 256 KiB and
nothing more: 64 such connections hold 16 MiB of the worker for as long as
their deadline, about twice the worker's own memory.
**Context:** From the performance review of the PR #82 fixes; the bound is
documented, its cost is not.
**Effort:** S
**Priority:** P3
**Depends on:** A measurement showing it matters

### A deterministic test for the remote detach's leave

**What:** Test at the CLI, with a fake QUIC remote that answers
`AttachLeave` late, that agt does not close its connection before the
worker has the leave.
**Why:** `cmd/agt`'s TestRemoteDetachHandsTheSizeOn catches a lost leave
only by luck: with the flush removed it failed 5 times in 25.
**Context:** From the testing review of the PR #82 fixes;
`internal/cli/retry_test.go`'s fake remote is the pattern.
**Effort:** S
**Priority:** P3
**Depends on:** None

### A leave that loses to a failed write

**What:** When the worker's write to an attachment fails, read the requests
the client already sent before taking it for a cut, so an `AttachLeave`
among them still counts.
**Why:** A client that leaves while output is flowing can be taken for a
cut: the size stays reserved for 10 s for a reattach that never comes, and
agent connections wait as long. `internal/worker`'s
TestOwnerLeavingPromotesMostRecentlyUsed fails this way about 2 runs in
100 on Linux, on main as well.
**Context:** Found while checking a Linux test failure in the review of the
PR #82 fixes. The attach loop selects between output and requests, and an
output write that fails returns before a queued leave is read.
**Effort:** S
**Priority:** P2
**Depends on:** None

### Write queued events before a handoff's exec

**What:** Drain the worker's event recorder before a live handoff execs the
new binary.
**Why:** Events still queued at the exec, the counts of held-back SSH agent
refusals among them, are lost.
**Context:** From the red-team review of the PR #82 fixes; true of every
event, not only the SSH agent's.
**Effort:** S
**Priority:** P3
**Depends on:** None

## Completed

### Keep the echo paused when input pauses it during a field probe

**What:** Have `fieldAnswer` stay paused and ask again when an unpredicted key
or a click arrived after the field was asked for.

**Completed:** echo-review (2026-10-22): a pause or resize makes the
outstanding field stale (`epoch` in internal/cli/echo.go).
