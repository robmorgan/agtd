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

## Completed
