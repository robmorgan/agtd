# Architecture

`agtd` is a local daemon that owns coding-agent sessions: their PTYs, processes, terminal state,
and metadata. `agt` is a thin client that sends requests over the resolved runtime root's
`agtd.sock` and prints or streams the responses.

```text
agent (Go CLI) ──unix socket──► agtd serve (Go) ──unix socket──► agtd session-worker (Go) ──PTY──► agent process
                                session registry,                   one per session: PTY owner,
                                request/attach proxy                shadow terminal, client fan-out
```

* The daemon (`internal/daemon`) owns the session registry in SQLite, spawns one worker per
  session, and proxies client requests and attach streams to it.
* Each session worker (`internal/worker`) owns one PTY and child process, feeds the output
  through `libghostty-vt`, and fans it out to attached clients over its own socket in
  `<runtime-root>/sessions/<id>.sock`.
* Control traffic and attach streams use a framed binary protocol.

## Code Layout

Both binaries are one Go module at the repository root.

| Package | What it does |
|---|---|
| `cmd/agtd` | `serve [--daemonize]`, `upgrade`, `remote enable\|disable\|status` (set `[remote] listen` and restart the daemon), `remote pair\|id\|list\|authorize\|revoke`, `bench sessions` (`internal/bench`: measures sessions on a private daemon), `session-worker` (also `--handoff-probe` and `--resume-fd`, for live handoff). `upgrade` replaces the daemon, then hands running sessions' workers over to the new binary. The agt CLI runs `serve --daemonize`. |
| `cmd/agt`, `internal/cli` | The `agt` CLI: commands, the inbox (bare `agt`), attach and the Ctrl-Y overlay (plain ANSI), and `hosts.toml` for remote hosts. Pure Go: it never imports `worker`, `daemon` or `db`, and `cmd/agt`'s tests check that, then drive both real binaries through PTYs. |
| `internal/daemon` | `agtd serve`: lock/socket/pid file lifecycle, create/kill/rm/ls/get, attach and request proxies to workers, history, daemon management, worker supervision and startup reconciliation. Tests run the daemon in-process against real worker processes. |
| `internal/worker` | One session: PTY via `creack/pty`, shadow terminal via `go.mitchellh.com/libghostty`, per-session Unix socket, activity and attention detection from the PTY stream, and live handoff to a new binary (`handoff.go`; OS-specific waits in `sys_linux.go`, `sys_darwin.go`). Real-PTY tests run under `-race`. |
| `internal/repo` | Read-only views of the git repository a session works in, for the daemon: git state, the diff and patch artifacts. Runs the system `git`; tests use real temporary repositories. |
| `internal/transport` | The seam between the protocol and the network: `Stream` (one request or attach session), `Listener`, the shared accept loop, the Unix socket transport, and the QUIC transport with pinned-key identities (`quic-go`). `transporttest` has a UDP relay that tests use as the network. |
| `internal/protocol` | The framed binary protocol and the small daemon management protocol, used by both binaries. Golden-frame tests pin the bytes. |
| `internal/sshagent` | SSH agent protocol messages, for agent forwarding: framing with OpenSSH's 256 KiB bound, the filter of what a forwarded agent takes, key fingerprints, identities and sign requests, session bindings, host names from `known_hosts`. Pure Go, used by the worker and the CLI. |
| `internal/session`, `internal/paths`, `internal/config` | The session model (and name rules), runtime-root resolution, and `config.toml`, shared by both binaries. |
| `internal/db` | `state.db`: schema, the guarded session state transitions the daemon and workers use, and session events. Uses `modernc.org/sqlite` (pure Go). Only the daemon and its workers open it. |
| `internal/procstat` | A process's resource usage from the OS (libproc on macOS, `/proc` on Linux) and the Go runtime's own numbers, for session stats and the bench. |
| `scripts/` | The libghostty-vt build helper. |

A few details of the daemon and workers that the sections below do not cover:

* `kill` asks the worker over its socket to stop; the worker stops the agent's process group
  (SIGKILL after 5s), writes the history logs, records the session as exited and sends
  `SessionEnded` to attached clients.
* PTY input goes through a bounded per-session queue and writer goroutine, so an agent that stops
  reading input never stalls output.
* Attach is a byte pipe through the daemon, so the daemon buffers no PTY output beyond one copy
  buffer per attachment. Slow-consumer policy lives in the worker (`internal/worker/broadcast.go`;
  see "Slow Clients, Buffering And Backpressure").
* Worker stderr goes to `logs/<id>.worker.log`; a daemonized `serve` logs to `agtd.log` in the
  root (not `logs/`, where it could collide with a session named `agtd`).

## Sessions Belong To The Daemon, Not To Connections

A client connection is disposable. Disconnecting (or never detaching cleanly) only ends that
attachment; the agent keeps running and any later connection can reattach.

The daemon is disposable too. Each worker runs in its own process session, so stopping, crashing,
or restarting the daemon does not touch running sessions. A new daemon finds them again through
`state.db` and the per-session sockets. `agt daemon restart` is therefore always safe.

A session counts as live only if its worker accepts a connection on its socket. Pids recorded in
`state.db` are never trusted on their own: after a crash or reboot they may belong to unrelated
processes. A single daemon per root is enforced by an exclusive lock on `agtd.lock`, held for
the daemon's lifetime and released by the kernel however it exits; `agtd.pid` is informational.

The daemon supervises the workers it spawned: it reaps them and records a worker crash as a failed
session. Sessions whose worker disappeared while no daemon was running are marked
`unknown_recovered` at startup or on the next `ls`.

## Lifecycles

### Tasks and sessions

Today a task is a session: one run of one agent in one working directory, under a name, with one
record in `state.db` and one worker process. The roadmap's task concepts (attention, elapsed time,
exit status, git state) are columns of that record or read from its working directory, so a task's
lifecycle is its session's, and there is no separate task table. If a task later spans several runs
(a retry, a follow-up agent), each run will still be a session and the task the grouping above
them; the session stays the unit that owns a PTY, a process and terminal state. Attachments, a
client's view of a session, are described in "Sessions, Incarnations And Attachments".

### Session states

A session's `status` in `state.db` changes only through the guarded updates in `internal/db`. Both
the daemon and the session's worker write it: the worker the outcome it knows first-hand, the
daemon what it can only infer.

```text
                 agt new (the daemon inserts the record)
                      │
                      ▼
                 ┌──────────┐   cwd missing, spawn failed, worker not up in 5 s,
                 │ creating │── worker exited first, or the daemon restarted ───► failed
                 └────┬─────┘
                      │ worker: agent spawned under the PTY, socket bound
                      ▼
                 ┌──────────┐   agent exits 0, or after kill ──────────────────► exited
                 │ running  │── agent exits non-zero or by a signal ──────────► failed
                 └──────────┘── worker crashes while its daemon supervises it ─► failed
                      │
                      │ worker gone, found by probing its socket
                      ▼ (it died while no daemon supervised it)
              unknown_recovered

  agt rm (or kill --rm): stops the session if it runs, then deletes the record and logs
```

* `creating`: the daemon has allocated the name and UID and is starting `agtd session-worker`.
  The worker passes the row's creation time to `MarkRunning`, which applies only to that
  incarnation and only while it is still `creating`, so a worker the daemon gave up on, or whose
  session was removed and recreated meanwhile, cannot claim it.
* `running`: the worker spawned the agent and bound `sessions/<id>.sock`. Only a worker that
  accepts a connection on that socket counts as live; recorded pids are informational.
* `exited`: the agent exited with status 0, or exited after a kill (the worker records a deliberate
  stop as `exited` whatever the status). The daemon also records `exited` if a worker it asked to
  stop disappears without recording an outcome.
* `failed`: the worker recorded why it could not start (missing directory, spawn or socket
  failure) or that the agent exited non-zero or by a signal; or the daemon recorded a failed start,
  a creation interrupted by a daemon restart, or a worker it spawned that died without recording an
  outcome (`MarkWorkerLost`, keyed on that worker's pid).
* `unknown_recovered`: the record says `running` but nothing listens on the socket, so the worker
  died while no daemon supervised it (SIGKILL, a reboot). This is found at daemon startup or on the
  next lookup (`ls`, `status` and the like), and is set only over `running`, so it never overwrites
  an outcome a worker wrote on its way out.

`exited`, `failed` and `unknown_recovered` are final: a session never restarts. Its history stays
readable (`agt history` reads the logs the worker wrote at exit) until `agt rm` deletes the
record and logs; the name can then be used again, by a new incarnation with a new UID.

### Daemon restarts

The daemon holds no session state that is not in `state.db` or in a worker, so restarting it is
safe:

* Workers run in their own process sessions and keep running, with their agents, PTYs and
  terminal state, while no daemon runs. A worker records its agent's exit in `state.db` itself.
* Stopping the daemon closes its client connections, which ends every attachment it was proxying
  (CLIs, local and remote, reconnect and reattach by themselves). Sessions are untouched.
* A starting daemon takes the lock, removes a stale socket, and reconciles: `running` sessions
  whose workers answer stay running and are reachable again; those whose workers are gone become
  `unknown_recovered`; sessions still `creating` become `failed` (a worker still starting is then
  refused by `MarkRunning`, stops its agent and exits).
* A daemon supervises only the workers it spawned. A worker that crashes under a later daemon is
  found by probing its socket, so its session becomes `unknown_recovered` rather than `failed`.
* `agt daemon restart` stops the daemon even with sessions running. A plain shutdown request is
  refused while sessions run. `agt daemon upgrade` replaces the daemon and then hands each running
  worker over to the new binary (see "Live Handoff").
* A worker that claims its session (`MarkRunning`) while a starting daemon reconciles keeps it:
  reconciliation fails only rows still `creating`, so a session is never recorded failed while its
  worker runs. A daemon also leaves the outcome of a worker it spawned to that worker's supervisor,
  which records a crash as `failed`, rather than probe it as `unknown_recovered` first.
Workers are not tied to a binary either: an upgrade hands each one over to the new executable
while its agent keeps running (see "Live Handoff" below).

## Restart Guarantees

What survives each kind of restart or crash, and how the next daemon reconciles what it finds.
`state.db` holds every session's metadata (name, agent, cwd, workspace, status, pids, exit
state, attention); the live PTY, agent process and terminal state live in the session's worker.

| Event | Survives | Lost | Reconciled by |
|---|---|---|---|
| Daemon restart (`agt daemon restart`, SIGTERM) | Every session: workers, agents, PTYs, terminal state, metadata | Open connections: attachments and control streams end (CLIs reattach and repaint), and a one-shot request in flight fails (the CLI never resends one it wrote) | The new daemon reads `state.db`; running sessions whose socket answers stay running |
| Daemon crash (SIGKILL, panic, OOM) | Same as a restart | Same as a restart, plus supervision of the workers it spawned: a worker that dies later is found by a socket probe (`unknown_recovered`) rather than reaped (`failed`) | The kernel releases the lock; the next daemon removes the stale socket and reconciles as above. A session it was creating is either never inserted, `failed` ("agtd stopped while the session was starting", and its worker, refused by `MarkRunning`, kills its agent and exits), or `running` if the worker got there first |
| Worker crash (SIGKILL) | Metadata | The session: its PTY is hung up, so the kernel sends the agent SIGHUP; terminal history not yet written to `logs/` (it is written when a session ends) | Recorded `failed` by the daemon that spawned the worker, or `unknown_recovered` when a probe finds its socket refusing. An agent that ignores SIGHUP survives, orphaned, with a dead terminal: agtd does not signal recorded pids, which may have been reused |
| Agent exits or crashes | Metadata, history (written to `logs/`) | The live session | The worker records `exited` (exit 0, or a kill) or `failed` with the reason, and sends `SessionEnded` to attached clients |
| Machine reboot | Metadata, history of ended sessions, keys, config | Every live session (processes and PTYs), and the unsaved history of those sessions | Sessions recorded running are `unknown_recovered` once the next daemon finds their sockets refusing or gone. On Linux, `install.sh`'s systemd user unit (with lingering) starts that daemon at boot, so remote clients can reconnect without a login; elsewhere the next `agt` command starts it |
| Upgrade with live handoff (`agt daemon upgrade`, `agtd upgrade`) | Everything a daemon restart keeps, and the worker itself moves to the new binary: same pid, same agent (still its child), same PTY, socket path, exact terminal state, attach numbering; queued input reaches the agent first, and output written during the handoff waits in the PTY | Attachments end with `SessionRestarting` (clients that do not know it are detached instead); a connection arriving during the exec just waits for the new image | The new image restores the state and touches the session's row (`MarkResumed`); nothing else changes in `state.db` |
| Upgrade without handoff (the handoff was refused or failed before the exec) | The session, unchanged, on its previous binary | Attachments, if the failure came after clients were told to restart (they reattach to the same worker) | Nothing to reconcile; `agtd upgrade` names the session and the reason |
| New image dies after the exec | Metadata | The session, as for a worker crash | As for a worker crash; `agtd upgrade` reports the session as lost |

**Downgrades are one-way past a schema bump.** A binary older than `state.db`'s version refuses
to open it (remove the runtime root to start fresh), and whichever process opens the database
first — the daemon, a worker, even `agtd bench` — migrates it. Two subtler costs of rolling
back a binary across the program status release (schema v6): a pre-v6 daemon still parses
activities strictly, so running workers writing the new words (`blocked`, `done`, `error`) make
it fail to read those sessions at all, not just miss their status fields; and a live handoff to
a pre-status image silently drops the program status records (the old image ignores the unknown
handoff field), so a blocked agent reads as whatever the old image's heuristics make of it until
it reports again — nothing is corrupted, but the program's word is lost. Roll workers and daemon back
together, or not at all.

Tests: `TestSessionSurvivesDaemonRestart`, `TestDaemonSIGKILLWithLiveSessions`,
`TestCrashDuringCreate`, `TestWorkerSIGKILLHangsUpTheAgent`, `TestWorkerLostWhileDaemonDown`,
`TestRecycledPidsAreNeverSignalled` and the handoff tests in `internal/daemon` (all against real
workers and PTYs), and `TestLocalAttachSurvivesUpgradeAndRestart` end to end.

## Live Handoff (Upgrading Workers)

A worker runs the binary it was started from, so upgrading the daemon alone leaves every running
session on the old code. `agtd upgrade` therefore replaces the daemon (stop, start the new
binary, wait for it), then sends each running session's worker `HandoffSession` with the new
executable, one session at a time, and reports each outcome. The request is a worker message: the
daemon refuses it from clients. A worker that predates handoff does not list `worker-handoff` in
its answer to `Hello` and is left alone.

The worker re-executes itself in place (`execve`). Two designs were prototyped:

* **Exec in place (chosen).** The pid does not change, so the agent stays the worker's child and
  its exit status can still be collected with `wait4`. The PTY master, the listening socket and the
  connection that asked for the handoff are inherited by clearing close-on-exec; the rest of the
  state travels in an unlinked temporary file (in `sessions/`) whose descriptor is inherited too.
  The socket path never stops listening, so the daemon never sees the session as dead, and
  connections made during the exec wait in the socket's backlog for the new image. The risk is
  that there is no way back once the exec has happened, so the old image first runs
  `<exe> session-worker --handoff-probe` (the binary must run here and resume this state format),
  and the new image falls back to a lossy terminal restore rather than give up.
* **SCM_RIGHTS to a new process (rejected).** Starting a second worker and passing it the PTY and
  socket over a Unix socket works at the descriptor level (`internal/worker/fdpass_test.go`), but
  the agent would not be the new worker's child: its exit status would be lost to the new worker
  (only the old worker, or init after it exits, may reap it), which could only poll for the exit
  (pidfd, kqueue `NOTE_EXIT`) and never learn the code, and for a while two processes could read
  the PTY. Exec in place has none of these problems.

The old image's steps (`internal/worker/handoff.go`), each undone in reverse if a later one fails,
including the exec itself:

1. Probe the new executable.
2. Hold back reaping the agent: the agent watcher waits for the exit without reaping
   (`waitid(WNOWAIT)` on Linux, kqueue `NOTE_EXIT` on macOS) and reaps under a lock the handoff
   holds until the exec, so the exit status is never collected by an image that is about to
   disappear; an agent that has already exited calls the handoff off.
3. Stop accepting connections, keeping a duplicate of the listening socket, and the same for the
   SSH agent socket, whose open connections are closed (an ssh in the middle of authenticating
   fails once).
4. End every attachment with `SessionRestarting` and wait for every connection handler.
5. Stop the PTY pump. The pump waits for the PTY to be readable (`poll`, or `select` on macOS,
   whose `poll` does not support terminals) before each read rather than blocking in `read`, so it
   stops without consuming anything; from here on the agent's output stays in the kernel's PTY
   buffer (an agent that fills it blocks) for the new image to read.
6. Wait for queued input to reach the PTY, so no write is cut short and none is lost. An agent that
   is not reading its input calls the handoff off after two seconds.
7. Snapshot the shadow terminal and exec.

The new image (`session-worker --resume-fd`) restores the terminal, starts the owner, pump, agent
watcher and accept loop, and answers the waiting `HandoffSession` with `HandedOff` (worker pid,
executable, handoff count). A kill requested just before the exec is carried over.

A session with an SSH agent socket (see "SSH Agent Forwarding") passes its listener the same way,
so the path the agent's `SSH_AUTH_SOCK` names never stops listening, and writes its state as
format 2. A binary from before agent sockets resumes only format 1, so the probe refuses it for
such a session rather than leave the socket listening with nobody to answer, where every program
asking for keys would hang; a session without the socket is still written as format 1, which any
binary since handoff resumes.

The shadow terminal crosses the exec as libghostty's binary snapshot (`Terminal.Snapshot`,
decoded with `NewSnapshotDecoderBytes`): both screens and their scrollback, cursor, modes, title,
and, since the terminal tracks it (`WithContinuationMaxBytes`, 1 MiB), the parser's state in the
middle of an escape sequence or UTF-8 character, so the rest of a sequence the agent was writing
is parsed the same way after the handoff. Tests compare the terminals before and after (VT and
plain renderings, cursor, active screen, modes) across real execs, and check that continuous
output across five handoffs ends up in the shadow terminal exactly once and in order. libghostty
does not yet promise the snapshot format stable across versions (the move from Ghostty 81598385d
to f54340ad kept it byte for byte, apart from Kitty image records that an older image cannot
read), so the state also carries the VT rendering; a new image that cannot decode the snapshot
rebuilds the terminal from that (screen and scrollback text, most modes) and logs it. An
unfinished sequence longer than 1 MiB cannot be snapshotted, and the handoff is refused until it
completes.

Clients follow: the CLI asks for `session-restart` in `AttachSession` when the daemon's Welcome
lists it, and on `SessionRestarting` attaches again and repaints, like after a lost connection.
An attach request carrying capabilities is only forwarded with those the session's worker also
lists (the daemon asks it with `Hello`), since a worker left on an older binary would refuse fields
it does not know.

Known limits: a SIGTERM delivered to a worker in the instant between its last check and the exec
kills the new image before it handles signals, losing the session as for a worker crash (agtd
never sends workers SIGTERM itself). A handoff waits at most about three seconds for clients that
do not read their last frames, and is refused if they still have not.


## Transports

The protocol runs over any bidirectional byte stream that supports half-close. Each stream has one
role (a request, the control stream, an attachment, a transfer, an event subscription; see
"Streams and their roles" below). Everything above `internal/transport` sees
only that `Stream` and a `Listener` that yields streams, so the daemon does not know or care which
transport a client used. The daemon-to-worker link is always a local Unix socket.

Two transports exist:

* **Unix socket** (`agtd.sock`): local clients, always on.
* **QUIC** (off unless `[remote] listen` is set): remote clients. Each client holds one QUIC
  connection and opens one bidirectional QUIC stream per role instance: a control stream for its
  one-shot requests, one per attachment, one per transfer. Long-lived attachments, transfers and
  short requests are multiplexed without blocking each other. Both peers ping
  every 5 seconds to hold idle connections open, and each connection may have at most 256 streams. The daemon's first
  datagrams are 1200 bytes, QUIC's minimum, rather than quic-go's default 1280: Tailscale's
  interface MTU is 1280 including IP and UDP headers, so larger ones never leave it and the
  handshake times out. Path MTU discovery raises the size afterwards where the path allows.

QUIC flow-control windows are set explicitly in `quicConfig`: 512 KiB growing to 4 MiB per
stream, 4 MiB growing to 16 MiB per connection. A stream whose reader stops (an attachment whose
terminal is paused) blocks only its own writer once its window is full; its data is kept, not
dropped, and the connection's other streams keep flowing until about 8 streams have stalled at
once. The connection window is therefore also the most one client can make the daemon buffer,
however many of its 256 streams it opens. A client whose address changes mid-connection (a NAT
rebinding, Wi-Fi to cellular) keeps its connection: the daemon validates the new path and moves to
it. Streams have no priorities (quic-go has none) and QUIC datagrams are disabled: measurements
showed no need for the first, and no protocol message is replaceable enough for the second.
[BENCHMARKS.md](BENCHMARKS.md) has the numbers and the reasoning.

QUIC connections authenticate both ways with pinned keys inside TLS 1.3 (ALPN `agtd`), the way
SSH uses host keys and `authorized_keys`, with no certificate authority:

* The daemon's key is `remote/daemon.key` (Ed25519, created on first use). Its fingerprint is
  `SHA256:` plus the base64 SHA-256 of the public key, and is what clients pin.
* `remote/authorized_clients` lists the client key fingerprints allowed in. It is checked on every
  handshake, on every new stream, and every 5 seconds for each open connection, so revoking a key
  also closes that client's live connections, attachments included. TLS session tickets are
  disabled: a resumed session would skip the check. `agtd remote authorize|revoke` edit the file
  under a lock and replace it atomically, so concurrent edits are never lost.
* An authorized client has the same access as the local socket owner, except that only a local
  client may stop the daemon: a remote one could not start it again.
* Keys are created without ever replacing one that exists (written to a temporary file, then
  linked into place), so processes creating a key at the same moment all end up using the same one.

Pairing (`internal/transport/pair.go`) sets up both halves of that trust from a short code instead
of copying fingerprints between machines. `agtd remote pair` asks the daemon (a management
request on the local socket only) to open a pairing window. The daemon shows a 9-digit code, and
`agt host add NAME ADDRESS --code CODE` proves it knows the code:

* A pairing client connects with ALPN `agtd-pair` instead of `agtd`. The daemon completes that
  handshake for any client key, but only while a window is open, and serves such a connection
  nothing but the pairing exchange (protocol/pairing.go, framed like the management protocol so it
  works across main-protocol versions, with frames capped at 4 KiB since the peer is not
  authenticated). The connection is then closed, and the client reconnects normally under its
  now-authorized key. At most 4 pairing connections are served at once, each for at most 30
  seconds.
* The two ends run CPace, a balanced PAKE (`filippo.io/cpace`, over ristretto255), with the code as
  its password. Its context holds both keys' fingerprints and a value exported from this TLS
  connection, so a machine in the middle, which must terminate TLS under its own key with each end,
  gets a different shared secret from each. An attacker learns nothing from the exchange that would
  let it test codes offline: each attempt is one online guess.
* The client confirms the shared key first (an HMAC over it). Only then does the daemon authorize
  the client's key under the name it sent (`authorized_clients`, as `remote authorize` would) and
  send its own confirmation. The client pins the daemon's key only after checking that confirmation.
* There is one window at a time. It closes when a client pairs, after 3 wrong codes, after 5 minutes,
  or when the `remote pair` command goes away, since the window belongs to its stream. With 30 bits
  of code and 3 guesses per window, a guess succeeds about 3 times in a billion.

`filippo.io/cpace` is small (about 200 lines) and pinned at its only release, which its author
calls experimental. Its construction is the one in draft-haase-cpace with HKDF and ristretto255,
and our protocol adds the key confirmation it leaves to callers. If a maintained, standardized
CPace for Go appears, it can replace this one: only `pair.go` uses it, and pairing is not
persisted, so changing it affects only CLIs and daemons that pair across that change.

The `agt` CLI is the client (`internal/cli/client.go`, over the same `internal/transport`
QUIC code as the daemon). Its key is
`remote/client.key`, in the same format as the daemon's, and `hosts.toml` maps host names to an
address and the daemon fingerprint pinned by `agt host add`. One CLI process opens at most one
QUIC connection, with one control stream for its requests and one stream per attachment or
transfer, so an attachment and the overlay's requests share a connection. A remote host is never started, restarted or upgraded from the CLI.
With TLS 1.3 a client can finish its half of the handshake
before the daemon rejects its key, so a refusal may only surface on the first stream; the CLI
recognises the TLS alert there and says how to authorize the machine. A name that resolves to
several addresses (`localhost` is often `::1` and `127.0.0.1`) is dialled at each in turn within a
10-second budget. A test pins one key's fingerprint, since `hosts.toml` and `authorized_clients`
store fingerprints and they must never change.

`agtd remote enable` picks the listen address when none is given: first a Tailscale address,
used without asking; otherwise the source address of the default route, found by connecting a UDP
socket (which sends nothing), which it asks about first, since that is a LAN address that may
change, a shared carrier-grade NAT address, or a public one. An address in Tailscale's ranges
(`100.64.0.0/10`, `fd7a:115c:a1e0::/48`) only counts as Tailscale's when Tailscale confirms it: it is
on the `tailscale0` interface, or `tailscale ip` reports it (macOS names the interface `utunN`).
Carriers and other VPNs hand out `100.64.0.0/10` addresses too.

`enable` edits only the `listen` line of `config.toml`, keeping comments and following a symlinked
file. The edit is checked before it is written: it must parse and leave every other setting as it
was, or the file is left alone. If the restarted daemon does not listen on the new address, the
previous setting is restored. A daemon whose listen address cannot be bound, or whose listener
stops, keeps local service and binds again every 5 seconds until shutdown, reporting the reason in
its management status.

A client that disappears without closing (a laptop lid, a lost network) is noticed once the daemon
has heard nothing from it for 15 seconds (`transport.DeadPeerTimeout`); until then its attachment
is still listed. Closing the connection ends its attachments; the session is unaffected. QUIC uses
the smaller of the two peers' idle timeouts, so this holds for older clients too. Both ends notice a
silent peer this way, so an outage longer than that ends the attachment even if the network comes
back.

The CLI hides that: when an attach stream fails because its connection was lost
(`transport.IsConnectionLost`: an idle or handshake timeout, an unreachable network, or the daemon
closing the connection, say to restart), it stays in raw mode, dials a new connection and attaches
to the same session again, then repaints from the new snapshot. Session identity never depended on
the connection, so nothing on the daemon is resumed. The reattach names the session incarnation it
expects (its UID, so it can never land on a new session that reused the name) and the attachment it
replaces, which the worker drops at once rather than when the daemon notices the old connection is
dead (see "Sessions, Incarnations And Attachments"). Retries back off from 0.5 to 5 seconds and
continue until they succeed or fail for a reason a retry cannot fix (a refused or changed key, a
session that is gone or was replaced, a protocol error). Meanwhile the bottom row shows the status,
and typed input is dropped rather than delivered late; only the detach key acts. Output written
while disconnected is not replayed; the snapshot shows the screen as it is now.

Local attachments do the same when the daemon goes away (restarts, upgrades or crashes): the
stream ends without a final frame, and the CLI retries the socket until a daemon answers, starting
one itself, as any command would, if none is back after a few attempts. Both also reattach when
the session's worker restarts for an upgrade (`SessionRestarting`, see "Live Handoff"), through
the same path, with the expected UID and the attachment being replaced.

A reattach never turns a session's end into "not running". A write (a key, a resize) that fails
because the worker closed the stream, after `SessionEnded` (the overlay's Stop, or input racing
the session's end) or after ending the attachment (a detach from elsewhere), is explained from the
frames already sent: the CLI reports the end or the detach rather than reconnecting. Only a stream
that ends without a final frame, or goes quiet for a second, counts as dropped and is reattached.
The daemon answers an attach that names the UID of an incarnation that has ended (`exited`,
`failed`, `unknown_recovered`) with that incarnation's `SessionEnded`, so a reattach that loses the
race learns how the session ended; a plain attach by name is still refused. A remote attach stream
that ends on its own while its QUIC connection lives on (`transport.IsConnectionLost` is false)
reattaches on that connection first, and dials a new one only if that try fails.

A path change that QUIC can follow needs no reconnect at all: when a client's address changes
mid-connection (NAT rebinding, or Wi-Fi to cellular behind the same NAT), quic-go validates the new
path and moves the connection to it, and every stream on it carries on. A reconnect is only needed
when the old path dies outright, and the attach above copes with that.

One-shot requests recover the same way. A remote request whose connection is lost after it was
sent is sent again on a new connection, with the same backoff, if that is safe: a read always, and a
request with side effects (`new`, `kill`, `rm`, `workspace add|rm`) only because it carries a
request token the daemon recognises (see "Duplicate And Replayed Requests"). Other requests
(`send-input`, `detach`) are never sent twice. A daemon that was never reached fails at once.

The daemon's tests run full sessions over QUIC, and over a TCP stand-in, to keep the seam honest.
The QUIC ones also run through a UDP relay standing in for the network
(`internal/transport/transporttest`), which can black-hole traffic, delay it by a chosen one-way time, block new connections, or move
a client to a new address: they cover an address change mid-attachment, a hard network switch, an
outage longer than the dead-peer timeout, a client that stops reading while its session writes
about 67 MB (checking the daemon's heap and the worker's RSS stay flat), and fifty abrupt reconnects in a row, counting goroutines, file descriptors, streams and attachments
before and after.

## Wire Protocol

`agt` and `agtd` communicate over a custom framed binary protocol, implemented in
`internal/protocol` and used by both binaries. Golden-frame tests pin its bytes, so a CLI and a
daemon of different builds, possibly on different machines, stay compatible. The current version
is 1.

Each frame has a fixed 16-byte header followed by a payload:

* `magic` (`u32`, little-endian) identifies an `agtd` protocol frame
* `version` (`u16`, little-endian) is the protocol version the frame is written in
* `message_type` (`u16`, little-endian) identifies the request or response variant
* `flags` (`u16`): bit 0 marks a frame whose payload starts with a `u32` request id (only on
  control streams, below). A frame with a flag the reader does not know is refused, since flags
  change the payload's layout
* `reserved` (`u16`, currently unused and set to `0`)
* `payload_len` (`u32`, little-endian) gives the number of payload bytes that follow (at most
  64 MiB; a longer declared length is refused before anything is allocated)

Payloads are binary-encoded field-by-field rather than serialized as JSON:

* strings => `u32 len` + UTF-8 bytes (invalid UTF-8 is refused)
* byte blobs => `u32 len` + raw bytes
* booleans => single `u8`
* optional values => presence `u8` followed by the encoded value
* lists => `u32 count` followed by elements (the count never sizes an allocation by itself)

PTY snapshots, PTY output, and interactive input are sent as raw bytes. A client on an unsupported
version gets an `Error` frame in its own framing explaining the mismatch, so the `Error` message
kind and its payload never change. Fuzz tests (`internal/protocol/fuzz_test.go`) check that no
input makes a decoder panic or allocate without bound, and that whatever decodes re-encodes to the
same value.

A second, deliberately tiny daemon management protocol (framed at header version 0, JSON payloads)
carries daemon status and shutdown for `agt daemon info`, `restart` and `upgrade`, so those keep
working even when the CLI and daemon speak different versions of the main protocol. Its status
includes connection-level metrics: the streams the daemon is serving, and for each open QUIC
connection the client key, remote address, stream counts, QUIC's smoothed RTT, bytes sent and
received, and packets lost (`agt daemon info`, `agtd remote status`).

### Streams and their roles

The unit of the protocol is a stream (a Unix connection, or one QUIC stream on a client's QUIC
connection). The first frame a client sends on a stream decides its role for its whole life, so no
separate negotiation round trip is needed (`protocol.Request.Role`):

| Role | First frame | Then |
|---|---|---|
| request | any one-shot request | one response, then the stream ends |
| control | `Hello` | `Welcome`, then any number of tagged one-shot requests and their tagged responses, in any order |
| attach | `AttachSession` | `Attached` with a snapshot, then `PtyOutput` (and `AttachResync`, `AttachSizeOwner`, `Field`, if negotiated) frames one way and `AttachInput`, `AttachResize`, `AttachSnapshot` (and `AttachTakeSize`, `AttachLeave`, `AttachField`) the other, until either side ends it |
| events | `SubscribeEvents` | `Event` frames, server to client, in id order: the backlog the request asked for, then live ones, until the client closes the stream |
| artifact | `GetArtifact` | `ArtifactChunk` frames, then `EndOfStream` (or an `Error`: the artifact is incomplete). On a stream of its own, a large transfer never delays the control stream or an attachment |
| artifact | `GetHistory` | one `History` response holding the whole history (the `history` artifacts stream the same content) |
| ssh-agent | `ForwardSSHAgent` | `SSHAgentForwarding` (or `Error`), then `SSHAgentRequest` and `SSHAgentClose` from the worker and `SSHAgentReply` and `SSHAgentClose` from the client, until either side ends it or its attachment ends (see "SSH Agent Forwarding") |
| management | a version-0 frame | one JSON response |

This is the stream taxonomy, and each kind of traffic has its place in it: small commands share the
control stream; each interactive attachment, being latency-sensitive and long-lived, has a stream of
its own; notice that something happened flows on events streams; and anything large (history,
diffs, patches, logs) goes on an artifact stream, so a big transfer never sits in front of a
keystroke or a small request. Over QUIC each stream has its own flow control, so
a stalled attachment or a large transfer never blocks another stream on the same connection, and a
client never needs more than one connection.

### Handshake, versions and capabilities

`Hello` is the connection handshake. The client sends the range of protocol versions it speaks
(framed at the lowest), its name, and the optional features ("capabilities") it supports. The
daemon answers `Welcome` with the newest version in both ranges, its own capabilities, and a
description of its machine (host name, OS, architecture, CPUs, memory, configured agents and the
default one), or with an `Error` when the ranges do not overlap. Both sides then use only the
features both listed. Today there is one protocol version (1), and these capabilities:

| Capability | Adds |
|---|---|
| `control-stream` | tagged requests on the stream that started with `Hello` |
| `git-state` | `GetGitState` |
| `artifacts` | `ListArtifacts`, `GetArtifact`, `ArtifactChunk` |
| `session-uid` | the session UID appended to session records and to `CreateSession`'s answer; on an attach stream, `AttachSession.ExpectUID` and `Attached.SessionUID` |
| `request-tokens` | a request token appended to `CreateSession`, `KillSession`, `AddWorkspace`, `RemoveWorkspace` |
| `attach-features` | `AttachSession` may end with the attach stream's own feature list (below) |
| `attach-replace` | attach stream: `AttachSession.Replaces`, the attachment this one replaces |
| `attach-resync` | attach stream: the worker may send `AttachResync` |
| `runtime-stats` | `GetSessionStats`, `GetDaemonStats` |
| `daemon-id` | the daemon's id appended to `Welcome` |
| `events` | events streams (`SubscribeEvents`), `ListEvents` |
| `session-activity` | the activity fields appended to session records (after the UID): activity, foreground command, title, last output and attention times |
| `session-status` | the program status fields (OSC 7501) appended to session records after the `session-activity` fields: app, kind of block, message and progress |
| `session-restart` | attach stream: the worker may end it with `SessionRestarting` when it restarts for a live handoff |
| `worker-handoff` | `HandoffSession` and `HandedOff`, between agtd and a session worker only (a worker lists it in its answer to `Hello`) |
| `terminal-memory` | the shadow terminal's memory appended to `SessionStats` |
| `attach-scrollback` | attach stream: `AttachSession.ScrollbackRows` caps the scrollback in the attach snapshot (see "Bounded scrollback") |
| `attach-size-owner` | attach stream: `AttachSession.TakeSize`, `Attached.SizeOwner`, `AttachTakeSize`, `AttachLeave` and `AttachSizeOwner`; attachment records carry their sizes after the list (see "Size ownership") |
| `attach-field` | attach stream: `AttachField` asks where the program's input field is, answered with `Field` (see "The input field and local echo") |
| `terminal-colors` | `CreateSession.Colors` and, on an attach stream, `AttachSession.Colors` carry the client terminal's default colours; the worker answers the program's OSC 10 and 11 queries with the most recent ones itself (see "Terminal queries") |
| `attach-frames` | attach stream: the screen arrives as `AttachFrame`s instead of `PtyOutput`, each acknowledged with `AttachFrameAck`; `Attached.Snapshot` holds the scrollback rows only and `AttachSnapshot` is answered with a full frame (see "Screen frames") |
| `attach-frame-window` | attach stream, with `attach-frames`: the worker sizes the frame window from the round trip (2 to `MaxFrameWindow`, 16), and every `AttachFrame` carries a trailing `Acked` u32, the last ack the worker had read when it built it (see "Screen frames") |
| `attach-alt-record` | attach stream, with `attach-frames` and `attach-frame-window`: while an agent's transcript is on the alternate screen, every `AttachFrame` carries a trailing `Record` (`FrameRecord`: where the screen is in the worker's record of the rows that left its top, and the rows the client does not hold yet) after `Acked` (see "The transcript record") |
| `attach-compose` | `SendInput.Compose` (written only when set, since the daemon forwards the request to the worker in the base encoding) and, on an attach stream, `AttachCompose`: a whole prompt, or Enter alone, encoded by the worker as the program's terminal modes ask for input (see "Composed input") |
| `kill-expect-uid` | `KillSession.ExpectUID` appended after the token: the daemon refuses a stop or remove whose UID is not the session's current incarnation, so a remove confirmed in the inbox never deletes a newer session that reused the name |
| `ssh-agent` | `ssh-agent` streams: `ForwardSSHAgent` and the messages on them. A worker lists it in its answer to `Hello` only when its agent was started with the worker's agent socket as `SSH_AUTH_SOCK` (see "SSH Agent Forwarding") |
| `workspace-clone` | `CreateSession.Clone` (a presence byte, then `Workspace`, `URL` and `Path`) appended after `Colors`: a session that clones a repository with the client's forwarded agent (see "Cloning a workspace") |

The protocol grows without breaking older peers this way: a new message kind, or a field appended
to the end of an existing message or struct (a session record, even inside a list), comes with a
capability, and neither side sends it unless the other advertised that capability. The
capabilities both sides listed are the stream's `protocol.Features`, which tell the encoder which
optional fields to write and the decoder which to expect. Streams without a handshake use the base
encoding. Decoders reject unknown kinds, unknown flags and trailing bytes, so nothing is ever
silently misread. Changing an existing encoding needs a new protocol version.

Attach streams have no `Hello`, so `AttachSession` carries their handshake: when the daemon's
`Welcome` listed `attach-features`, the CLI appends the attach-stream capabilities it wants that the
daemon also listed (`session-uid`, `attach-replace`, `attach-resync`, `session-restart`,
`attach-scrollback`), followed by the fields of the
ones listed. The daemon cuts the list down to what the session's worker supports (it asks the worker
with a `Hello` on the worker socket; a worker started by an older daemon build may support fewer,
and one from before attach features supports none), and the worker's `Attached` echoes the list in
effect, followed by its fields. Each side uses only what that list names. An older CLI sends no list
and gets the base `Attached`; an older daemon never advertises the capability, so a newer CLI sends
none.

The CLI opens one control stream per process, right after connecting, and sends all its one-shot
requests on it (`internal/cli/control.go`). Each request carries a `u32` id that its response
echoes; the daemon runs up to 16 requests of one control stream at once and stops reading the
stream beyond that, so a client that pipelines requests is held back by flow control rather than
queued in the daemon. A request that cannot be decoded is answered with an error under its id and
the stream carries on. Requests that need their own stream (attach, artifacts, history) are refused on a
control stream. An artifact stream has no handshake of its own: the CLI opens one only after its
control stream's `Welcome` listed `artifacts`. If the control stream has ended when the CLI next uses it (the daemon restarted,
say), it opens a new one. A request it already wrote is resent only if that is safe: a read, or a
tokened request after a lost remote connection (see "Duplicate And Replayed Requests").

### Messages

| Request | Response | Role |
|---|---|---|
| `Hello` | `Welcome` | control |
| `GetDaemonInfo` | `DaemonInfo` | request |
| `ShutdownDaemon` | `Ok` | request |
| `CreateSession` | `CreateSession` | request |
| `KillSession` | `KillSession` | request |
| `GetSession` | `Session` | request |
| `ListSessions` | `Sessions` | request |
| `ListAttachments` | `Attachments` | request |
| `DetachSession`, `DetachAttachment` | `Ok` | request |
| `SendInput` | `InputAccepted`; with `Compose` (`attach-compose`) the data is a whole prompt the worker encodes for the program, Enter included | request |
| `ListWorkspaces` | `Workspaces` | request |
| `AddWorkspace` | `Workspace` | request |
| `RemoveWorkspace` | `Ok` | request |
| `ListEvents` | `Events` | request |
| `GetHistory` | `History` | artifact |
| `GetGitState` | `GitState` | request |
| `ListArtifacts` | `Artifacts` | request |
| `GetArtifact` | `ArtifactChunk` frames, then `EndOfStream` (or `Error`) | artifact |
| `GetSessionStats` | `SessionStats` (`runtime-stats`) | request |
| `GetDaemonStats` | `DaemonStats` (`runtime-stats`) | request |
| `SubscribeEvents` | `Event`, repeated | events |
| `AttachSession` | `Attached` (or `SessionEnded` for a finished session), then `PtyOutput`, `AttachSnapshot`, `AttachResync`, `AttachSizeOwner`, `Field`, and finally `SessionEnded`, `EndOfStream`, `SessionRestarting` or `Error` | attach |
| `AttachField` | `Field` (`attach-field`), on an attach stream; under `attach-frames` the next `AttachFrame` carries the field instead | attach |
| `AttachFrameAck` | nothing; the next `AttachFrame` once the screen changes (`attach-frames`), on an attach stream | attach |
| `AttachCompose` | nothing; the text and Enter are typed into the session as the program's modes ask (`attach-compose`), on an attach stream | attach |
| `HandoffSession` | `HandedOff` (from the new image) | request, agtd to a worker only |
| `ForwardSSHAgent` | `SSHAgentForwarding`, then `SSHAgentRequest` and `SSHAgentClose` frames, which the client answers with `SSHAgentReply` and `SSHAgentClose` (`ssh-agent`) | ssh-agent |

Any request may instead be answered with `Error`. Kinds are numbered by area so that parallel work
does not collide: 1-19 and 101-118 are the base protocol, 30-39 and 130-139 git state and
artifacts, 50-59 and 150-159 attach streams (`AttachTakeSize` is 50, `AttachLeave` 51, `AttachField` 52,
`AttachResync` 150, `AttachSizeOwner` 151, `Field` 152, `AttachFrameAck` 53, `AttachFrame` 153, `AttachCompose` 54), 60-69 and 160-169 worker handoff,
80-89 and 180-189 SSH agent forwarding (`ForwardSSHAgent` 80, `SSHAgentReply` 81, `SSHAgentClose`
82 from the client and 182 from the worker, `SSHAgentForwarding` 180, `SSHAgentRequest` 181).

`attach` is the bidirectional case. After the initial `AttachSession` request and `Attached`
response (which carries a snapshot of the current screen), the worker streams `PtyOutput` frames
while the client sends `AttachInput`, `AttachResize`, and `AttachSnapshot` frames on the same
stream until either side closes. The daemon forwards the attach stream as raw bytes and never
buffers PTY output itself.

## Events and Attention

`agtd` answers "which agent needs me?" with events. An event (`session.Event`) is something that
happened to a session: an id, the session, a time, a kind, an attention level (`info`, `notice` or
`action`) and a one-line summary. The kinds and their default levels are defined in
`internal/session/event.go`:

| Kind | Level | Recorded by, when |
|---|---|---|
| `created` | info | the daemon, when it creates the session row |
| `started` | info | the worker, once the agent runs in its PTY |
| `exited` | notice | the worker, when the agent exits with status 0 |
| `killed` | info | the worker (or the daemon), when the agent stopped on request |
| `failed` | action | the worker or daemon, when the session fails to start or the agent exits non-zero |
| `worker_lost` | action | the daemon, when a worker died without recording an outcome |
| `recovered` | info | a starting daemon, for each session whose worker kept running |
| `bell` | info | the worker, when the program rings the bell (observability only; never raises attention) |
| `notification` | info | the worker, for a desktop notification (OSC 9, OSC 777); the summary is its text (observability only) |
| `idle` | notice (info while a client is attached) | the worker, when the program reports it awaits the user's direction (OSC 7501) |
| `working` | info | the worker, when the program reports it is busy again (OSC 7501) |
| `stalled` | notice | the worker, after 30 minutes without output while a command other than the agent holds the PTY's foreground and nothing reports status |
| `blocked` | action | the worker, when the program reports it cannot proceed without the user (OSC 7501); the summary is the kind of block and the program's message |
| `done` | notice | the worker, when the program reports it finished (OSC 7501) |
| `error` | action | the worker, when the program reports it failed and stopped (OSC 7501) |
| `acknowledged` | info | the daemon, when the user looked at the session (below) |
| `ssh_agent_signed` | info | the worker, for each signature a forwarded SSH agent gave a program in the session: what was signed (a login, as whom, and to which host when the client's agent accepted a binding for the login's session; an SSHSIG namespace), the key and the client (see "SSH Agent Forwarding") |
| `ssh_agent_refused` | info | the worker, for a request a forwarded agent never takes (adding or removing keys, say), or a signature the client did not give; refusals held back are counted in a later one ("... (and 4 more like it)", or "refused 9 more requests, too many different ones to list each") |
| `ssh_agent_unavailable` | info | the worker, when a program found no agent: once per gap in forwarding, and only in a session that has been forwarded to (never raises attention) |
| `paired` | notice | the daemon, when a machine paired with a one-time code (host event) |
| `pair_failed` | notice | the daemon, when a pairing window closed after 3 wrong codes; the summary names the last address that guessed (host event) |
| `authorized` | notice | `agtd remote authorize`, when it adds a key (host event) |
| `revoked` | notice | `agtd remote revoke` (host event) |
| `connected` | info | the daemon, when an authorized remote client connects; at most once per key and address every 10 minutes, since every remote command connects (host event) |
| `disconnected` | info | the daemon, when it drops a live connection because its key was revoked (host event) |

Kinds travel as strings, so a client shows kinds newer than itself instead of failing to decode them.

**Host events** are about the machine rather than a session: who was allowed to reach the daemon
remotely, and who did. They have an empty session id, so the unfiltered feed (`agt events`,
`--all`) shows them next to session events, and the CLI names the host in the session column
(`host` for this machine). A session's own feed never includes them, and they raise no session's
attention. `agtd remote authorize` and `revoke` run outside the daemon, so they write their
events to `state.db` directly, as workers do; the daemon records the rest. Handshakes refused for
an unknown key are not recorded yet.

**Program status (OSC 7501) is the primary source.** A program that speaks the [program status
protocol](https://www.superlogical.com/rex/docs/build/program-status) reports what it is doing
directly: `idle`, `working` (with progress), `blocked` (with why: `permission`, `question`,
`auth`), `done` or `error`, with an `app` name, a one-line message, and hierarchical record ids
for programs doing several things at once. libghostty validates and parses the reports; the worker
keeps the records (`internal/worker/status.go`) under the specification's rules: a report replaces
its record whole, `clear` removes a record and its descendants (or everything), a new shell prompt
(OSC 133 A) drops `working`, `blocked` and `idle` records (that program is over; an idle record
left behind would pin the session while the next, possibly non-reporting, command runs), `done`
and `error` records stay until the user has seen them — typing into the session or attaching to
look at it (a reattach that replaces a lost connection is the CLI reconnecting by itself, so it
does not count) — a full reset removes all, and at most 256 are kept, evicting the least recently
updated. Reports and prompt starts are applied in stream order, so a report arriving after a
prompt in the same coalesced PTY read belongs to the next command and survives it. The records survive a live worker handoff (they travel
in `handoffState`, states as the specification's words): a blocked agent must stay blocked across
an upgrade, since the program will not re-report on its own. The event dedup memory rides along,
so the new image neither re-alerts a question the user already saw nor loses one the rate limit
had deferred at handoff time.

While any records exist, they decide the session's activity: the state that needs the user most
wins (`blocked` > `error` > `working` > `done` > `idle`), so one blocked shard of a parallel
program blocks the session and one still working keeps it working however many are done.
Transitions record the events above (repeats within 10 seconds are dropped; a blocked program
asking something new re-raises). Typing into the session counts a blocked record as answered —
silently — until the program reports again. Input made only of reports the client terminal sends
for a gesture or by itself (a wheel scroll, mouse motion, a focus change, a key release, a CSI
reply to a query) reaches the program but is not typing: it answers nothing and is not activity
(`isPassiveInput`), though it does count as the user having seen a done or error record; a click
or any key is typing. There is no inferred state: a session whose program
never reports shows as plainly `working` while it runs, with no judgment attached. Bells and
desktop notifications are recorded as info-level events for observability only (facts about what
the terminal emitted, useful when a reporter misbehaves), and never raise attention; the bell/
notification-to-waiting and output-timing-to-idle heuristics that predated the protocol were
removed once Claude Code adopted it. The worker answers the protocol's detection query
(`ESC ] 7501 ; ?`, echoed back) whether or not a client is attached, since it consumes the reports
itself; the identical reply of a client terminal that also supports the protocol is dropped from
the input, since the worker has already answered and a terminal's reflex must not count as the
user typing.

### Terminal queries

The other queries a program makes (cursor position, device attributes, keyboard flags, mode
states) are answered by the shadow terminal only while no client terminal is attached; with one
attached, the query is relayed and the terminal answers. The default colour queries (OSC 10 and
11) are the exception. libghostty has no default colours to answer with, and a remote terminal's
answer takes a round trip, which is more than the 250 ms Codex waits at startup before it gives up
on styling its composer. So `agt new` and `agt attach` ask their own terminal once, at startup
(OSC 10 and 11, then the primary device attributes as the sentinel every terminal answers, 300 ms
at most; `internal/cli/colors.go`), and send the answer with `CreateSession` and `AttachSession`
(`terminal-colors`). The worker keeps the most recent colours, answers the program with them at
once whether or not a terminal is attached, and removes the query from the output, so an attached
terminal does not answer a second time, late, with what the program would take for typed text
(`internal/worker/colors.go`; a query split across PTY reads is held for its end, at most
`groundTimeout`). The colours ride along a handoff. With none reported, the queries pass through
as before.

**Persistence.** Events are rows of the `events` table in `state.db`, written by the daemon and
the workers. Ids come from `AUTOINCREMENT`, so they increase per runtime root and are never reused;
because every write is an immediate transaction, ids are committed in order, and a reader that has
seen id N never later finds a new event below N. An id is therefore a cursor. Each session keeps
its newest 500 events, pruned in the transaction that inserts a new one, and removing a session
removes its events, so the table is bounded by the sessions retained. Host events are pruned as their
own bucket of 2000. A lifecycle change and its
event are written in one transaction.

**Attention.** A session record's `attention`, `attention_summary` and `attention_at` are the
highest-level unacknowledged event since the user last looked, and that event's summary. An event
raises the attention to its level unless something more urgent is pending (an equal level replaces
the summary; info events never change it). Lifecycle endings (exited, killed, failed, worker lost)
replace the attention instead, since an agent that has stopped is no longer waiting for whatever it
asked. Acknowledging resets the attention to info and records an `acknowledged` event, if there was
anything to clear. Attaching interactively (`attach`, or Enter on a session in the inbox)
acknowledges, and so does detaching from a session that is still running: either way the user has
seen its screen. A reattach that replaces a lost attachment is the exception: the CLI reconnected
by itself, no one looked, so attention raised while the connection was down stays pending. The
daemon writes an attach's acknowledgement beside the attach rather than
before relaying it, so attaching never waits on the database; it only clears attention raised
before the attach began (`db.AcknowledgeBefore`), so a session that fails at once keeps its
attention. An attachment that ends because the session ended does not, and output reaching
an attached client does not either, since an attached terminal may be a background tab. An ended
session's attention stays until it is removed.

**Activity.** The worker also keeps the session's `activity`: what the program reports (`working`,
`blocked`, `done`, `error`, `idle`), plain `working` for one that does not report (no judgment is
attached), and `exited` once the agent is gone (`waiting` appears only in rows written by older
workers that still inferred it from bells). Alongside it: the program's reported app name, kind of
block, message and progress (`status_app`, `status_kind`, `status_msg`, `status_progress`, NULL
for a program that does not report), the name of the process in the PTY's foreground (`tcgetpgrp`
on the master, sampled every second: the agent itself, or a command a shell runs there under job
control), the terminal title, and when the program last wrote output. It writes them on report
transitions, title and foreground changes only, never per output chunk, and never bumps
`updated_at`, so lists do not reshuffle as agents report and resume. (`last_output_at` is as
fresh as the latest such write.) Elapsed time is derived from `created_at` and `exited_at`. The
status fields travel to clients under their own capability (`CapSessionStatus`), appended after
the `CapSessionActivity` fields in session records.

**Detection.** Program status reports, bells, desktop notifications (OSC 9 and OSC 777), shell
prompts (OSC 133) and title changes come from libghostty's parser, through its effect callbacks,
out of the `VTWrite` the worker already makes for each PTY read; the added cost per chunk is a
clock read. The owner goroutine never touches the database for this: events and activity go to a
recorder goroutine through a queue of at most 64 events (dropped, and logged, while it is full)
and a single activity slot (a newer snapshot replaces an unwritten one). Noise is bounded: a bell
is recorded unless another was seen in the last 10 seconds with no input since, so a program
ringing in a loop records one event; a repeated notification text likewise; a bell within 2
seconds of a notification is the same request; and a program flipping between the same reported
states gains nothing within 10 seconds. The status
reports of one feed are not capped (replaying them into the record store needs them all, and a
report costs the program at least its own sequence bytes of output), while notifications are kept
to 4 per feed. OSC 99 (kitty's notification protocol) is not parsed by libghostty and is not
detected.

**Events streams.** `SubscribeEvents{AfterID, SessionID, Tail}` opens an events stream: the daemon
sends every retained event after `AfterID` (or, without it, the newest `Tail` events), then new
ones as they are recorded, until the client closes the stream. Each subscriber is a cursor over the
table: its handler reads at most 256 events after its cursor, writes them, and repeats, waiting
once it has caught up. The daemon thus holds at most one batch per subscriber and never buffers for
a slow one: a subscriber that stops reading blocks only its own handler, in a write, while events
accumulate in the table (one that falls behind past the retention limit skips what was pruned).
Workers write the table directly, so while anyone is subscribed the daemon polls the newest id
(`sqlite_sequence`) every 250ms and wakes subscribers when it changes; events the daemon records
itself wake them at once. A poll costs about 130µs (`BenchmarkLastEventID`, mostly opening the
connection), under 0.1% of a core, and none is made without subscribers. `ListEvents{AfterID,
SessionID, Limit}` is the one-shot form on the control stream (at most 1000 per answer).

**Clients.** `agt events [SESSION] [--follow]` prints events or follows them; following keeps
the id of the last event it handled and, when the stream ends (a daemon restart, a lost
connection), subscribes again after it, so nothing is missed or repeated. `--notify` rings the bell
and asks the user's terminal for a desktop notification (OSC 9, or OSC 777 on VTE terminals, foot
and urxvt; passed through tmux) for action-level events, and `--exec CMD` runs a command per event
with the event in `AGTD_EVENT_*` variables: these are the task-level notifications, and they work
the same against a remote host. `agt ls`, `agt status`, the inbox and the Ctrl-Y overlay show
attention and activity, and order live sessions needing action first.

## Creating a Session

When you create a session (`agt new [--cwd DIR] [NAME]`), the daemon:

1. Validates the name and agent, and resolves `cwd` on its own machine: an absolute path, a
   `~/` path expanded against the daemon user's home, or a path relative to a named workspace
   (stored in `state.db`, managed with `agt workspace`). The directory must exist; it does not
   need to be a git repository.
2. If `cwd` is in a git repository with a commit, reads HEAD's commit and branch: the session's
   base (see "Git State And Artifacts"). Nothing else about git is touched.
3. Allocates a session id (the name) and a new UID, and stores the record, base included, in
   `<runtime-root>/state.db`.
4. Spawns `agtd session-worker` in a new process session.
5. Waits for the worker to report the session running and bind its socket.

The worker spawns the configured agent inside a PTY in `cwd`, through the user's login shell
unless the agent's `login_shell = false` (`internal/worker/shell.go`): `$SHELL -l -i -c 'exec
"$0" "$@"' COMMAND ARGS...`, interactive because the PTY is a terminal. The daemon's environment
is fixed when it starts, and a daemon started by systemd, launchd or `agt` from a script never
read the rc files only interactive shells read (a stock Debian `~/.bashrc` returns at once
otherwise), where installers put `PATH` entries and credential variables. Running the shell per
session gives every new agent the environment of an ssh login, re-read each time, with no
restart. The `exec` keeps the shell's pid, so the agent is still the PTY session's leader and
process group (kills, reaping, the foreground watch and handoffs depend on it). The command
string sets `SSH_AUTH_SOCK` again after the rc files, as the worker pins it. A `$SHELL` the worker
does not know (or none) starts the agent directly. A failed `exec` is exit 127 or 126 from the
shell, which the failure message explains, since it is no longer a spawn error. Only
environments reach a running agent at its start: a value that rotates belongs behind a stable
pointer (a socket, a loopback URL, a file), as `SSH_AUTH_SOCK` and llmd-style credential proxies
do.

The agent gets `AGTD_SESSION_ID`,
`AGTD_SESSION_NAME` and `AGTD_CWD`, set before the shell runs so rc files can tell a session
apart, and `TERM=xterm-256color` and
`COLORTERM=truecolor` when the daemon's environment has no `TERM` (it was started by systemd or
over ssh) or has `TERM=dumb` (a login shell there): the agent's terminal is the worker's shadow
terminal whatever started the daemon, and without this an agent turns colour off. A `TERM` the
environment names on purpose is kept. The daemon socket is deliberately not passed in.
That is not a boundary: agents run as the socket owner and can still find the socket at its
well-known path, so isolating them needs a sandbox around the agent process. With `[ssh_agent]
session_socket` (the default) the worker also binds the session's SSH agent socket before the
agent starts and sets `SSH_AUTH_SOCK` to it (see "SSH Agent Forwarding"); without it the agent
inherits the daemon's `SSH_AUTH_SOCK`, which on a devbox is often missing or stale: that of
whichever ssh login started or upgraded the daemon. One that names another session's socket (a
daemon started from inside a session) is dropped either way.

The daemon does not manage git worktrees or branches. That responsibility sits with whatever starts
the session (the user, a wrapper, a skill, or the agent itself); the README shows the worktree
recipe.

Working directories are resolved by the daemon, never the client, because the client may be on
another machine. The local CLI still canonicalizes a plain `--cwd` itself, since it shares the
daemon's filesystem; with `--workspace` it sends the name and the relative path unchanged.

Workspaces are daemon state, not configuration: they live in the `workspaces` table of
`state.db` and clients add, list and remove them over the protocol, so a remote client can manage
them without touching the host's files. A session stores the workspace name next to the cwd
resolved from it at creation. Workspaces are addressing only: agtd does not sync them, and
creates one only by cloning it at a client's request.

### Cloning a workspace

`agt workspace add NAME [PATH] --clone URL` clones a repository on the daemon's machine with the
user's own keys and registers it. The clone is a session (`CreateSession.Clone`, under
`workspace-clone`), not something the daemon does by itself. That gives git a PTY for ssh's
first host key question and gives `SSH_AUTH_SOCK` the forwarder of the attachment `agt` opens at
once, as `agt new` does. It keeps a slow clone off the control stream and reuses the
request-token replay, history and events that sessions have. The daemon accepts SSH URLs only
(`ssh://` or scp-like, nothing git or ssh could take for an option), a valid and free workspace
name, and a `PATH` (`~/` expanded on its side) that does not exist yet. It refuses when
`[ssh_agent] session_socket = false`. It then runs `git clone -- URL PATH` from its user's home
through the login shell, in a session named `clone-NAME` (numbered when taken), with no base
commit. The worker runs it with `--ssh-agent-require-forwarder` (see "SSH Agent Forwarding"), so
the clone gets keys only from a client's forwarder, never the agent agtd inherited. A clone that
ends before its worker is seen ready (git failed at once) still counts as created: its end is its
result.

The client side decides whether forwarding works before anything starts. `agt` refuses unless
`[ssh_agent] forward` is set and sound and its agent answers with a key the policy offers,
through the agtd hop for a remote daemon, which is the check that decides whether it would open a
forwarder. It also refuses a daemon without `workspace-clone`, since an older one would drop the
field and start an agent. It then attaches, and on exit 0 it registers the workspace with
`AddWorkspace`. The daemon keeps no pending registration, so a detach mid-clone leaves that to
the user, and `agt` prints the command. A failed clone's error names its history, and when the
clone ended before the attach could show it, `agt` prints the history's last lines.

For each session there are three kinds of state:

- durable metadata in SQLite for status, working directory, attention, activity, exit state, and
  the session's events
- terminal state and scrollback in the worker's `libghostty-vt` instance, written to
  `logs/<id>.log` (VT) and `logs/<id>.rendered.log` (plain) when the session ends
- the worker's live PTY and output fan-out used by `attach` and `send-input`

PTY input can come from an interactive `attach` or a background `send-input`. Multiple clients may
attach to the same session concurrently; input is shared, and one of them owns the size (see "Size
ownership").

Killing a session asks the worker, over its socket, to stop (the worker handles SIGTERM the same
way). The worker stops the agent's whole process group (SIGKILL after five seconds), writes the
history logs, records the session as exited, and sends `SessionEnded` to attached clients. The
daemon signals a worker directly only if it is wedged, and only while its socket still answers.

## Git State And Artifacts

`agtd` does not manage git, but it reports what an agent produced in git, read from the
session's directory on the daemon's machine (`internal/repo`, `internal/daemon/artifacts.go`). It
only ever reads: the repository, its index and its refs are never written.

**Base.** When a session is created in a repository whose HEAD has a commit, the daemon records
that commit and the branch (`sessions.git_base`, `git_base_branch`, schema v3). What the session
produced is measured from there: its commits are those reachable from HEAD but not from the base,
and its changed files are everything that differs from the base: committed since, staged,
unstaged, and untracked. A session without a base (created before v3, outside a repository, or on
a branch with no commits yet) is measured from HEAD instead (or from the empty tree on an unborn
branch); one whose base has since vanished from the repository is reported as such and measured
from HEAD. The base is never moved: a rebase, a branch switch or a reset by the agent is reported
against where the session started.

**`GetGitState`** (a one-shot request) answers with the repository root, branch (or detached HEAD),
HEAD, base, upstream with ahead/behind counts, the commits since the base (hash, subject, author,
time) and the changed files (path, old path for renames, status, added and deleted lines, binary).
Lists are capped (200 commits, with the total count; 1000 files) and say when they were cut; git is
stopped as soon as enough has been read, so a huge repository costs a bounded read. Failures (git
not installed, the directory gone, a timeout after 30s) are reported inside the answer rather than
failing the request, so `agt status` always works. It works for ended sessions too, while their
directory exists.

**Artifacts.** `ListArtifacts` names what can be downloaded: `diff` (a unified diff of everything
since the base, untracked files included), `patch` (`git format-patch --stdout` of the commits since
the base: an mbox for `git am`), and `history` and `history.vt` (the terminal history, with its size
once saved). `GetArtifact` streams one on an artifact stream.

**How git runs.** The system `git`, in the session's directory, with `GIT_OPTIONAL_LOCKS=0`,
`core.fsmonitor=false`, no pager or colour, no external diff or textconv, and explicit flags for
everything a user's config could change (rename detection, `a/` and `b/` prefixes, no relative
paths). The daemon's own `GIT_*` variables are removed. Each git runs in its own process group,
killed as a group when its request ends or its client goes away.

`git diff` leaves out untracked files. To include them without touching the repository, the daemon
copies the index to a private temporary directory, marks the untracked files intent-to-add in the
copy (`GIT_INDEX_FILE`), and gives those commands a private object directory with the repository's
as an alternate, since intent-to-add stores the empty blob. Git then handles untracked files like
any other: binary detection and attributes apply, and a file moved without `git mv` is a rename. At
most 20,000 untracked files are included (`GitState` says when there were more).

**Streaming and backpressure.** An artifact is sent as `ArtifactChunk` frames of at most 256 KiB,
built in place around one reused buffer as git (or the history file) produces them, then
`EndOfStream`. The daemon never holds an artifact: a client that stops reading blocks the daemon's
write, which stops it reading git's stdout, which blocks git on its pipe. In memory per transfer:
one chunk, plus the pipe and the transport's buffers (over QUIC, the stream's flow-control window).
The transfer resumes when the client reads again and ends when it closes the stream, its connection
dies, or the daemon shuts down, killing git. There is no stall timeout, since a pager may pause as
long as its reader does. Live history is the exception: the worker answers with the whole history
in one frame (at most 64 MiB), which the daemon then sends in chunks.

Large transfers are separated from terminal traffic by giving each its own stream: over QUIC each
stream has its own flow control, so a transfer, stalled or at full speed, does not delay an
attachment on the same connection. `TestInteractiveDuringLargeArtifactOverQUIC` measures it on
loopback with a 48 MiB diff. Over six runs on a machine shared with other test suites, input
echoed through a live session over one QUIC connection took a p50 of 0.3-0.7 ms idle, 0.3-1.2 ms
with a stalled transfer open, and 0.35-0.9 ms while the transfer ran at 40-57 MiB/s (git diff
included), with a p99 of 1-12 ms and never more than 15 ms.

## Resource Usage

Each session's worker measures itself on request (`GetSessionStats`, which the daemon forwards):
its process from the OS (resident and private memory, CPU time, threads, open files) and its Go
runtime (goroutines, heap, memory held from the OS), the agent's top process, and its terminal and
fan-out (size, scrollback rows, PTY bytes and reads, chunks dropped for lagging clients). Asked
to, it also formats a reattach snapshot and replays it into a fresh terminal, timing both. The
daemon answers `GetDaemonStats` for its own process. `agt status --stats` and `agt daemon
stats` show them, and `agtd bench sessions` uses them to measure sessions at scale
(BENCHMARKS.md, "Sessions at scale").

Private memory is reported beside RSS because RSS counts the pages every worker shares (the
`agtd` executable and system libraries, about 11 MiB of each idle worker's 20 MiB), so summing
RSS over many sessions overstates their cost. libghostty's memory is outside the Go runtime; the
worker reports what its terminal holds as libghostty counts it (`Terminal.MemoryUsage`: pages,
resident bytes, and how many pages are compressed) to clients that negotiated `terminal-memory`,
shown by `agt status --stats` and in `agt daemon stats`' TERMINAL column. A new terminal holds
about 25 KB (`BenchmarkTerminalMemory`), a full scrollback about 9.4 MiB.

Quiet scrollback is compressed. On the worker's one-second tick, once no output has arrived for
three seconds, the worker runs libghostty's incremental compression in steps of at most 5 ms on
the owner goroutine, until it reports nothing more to do; it starts again only when the
scrollback changes or a snapshot has read it (formatting decompresses the pages it reads, without
changing libghostty's activity token). A full scrollback drops from 9.4 MiB to about 0.6 MiB
resident. A busy session is never compressed, since new output would decompress it again, and a
terminal restored after a live handoff is decoded with its history compressed
(`SnapshotDecoder.SetCompressHistory`) rather than held uncompressed.

## Sessions, Incarnations And Attachments

A session's id is its name. Names are unique among the sessions that exist, but a name is free
again once its session is removed, so a name alone cannot tell a client whether the session it
knew is the one there now. Every session therefore also has a UID: 128 random bits, generated when
the session is created, stored in `state.db` and never changed or reused. One UID is one
incarnation of a name. `agt status` shows it. When several hosts are known to one client, a
global id is the host's identity (its key fingerprint) plus the UID; neither part depends on the
name.

An attachment is one client's live view of one incarnation, from `AttachSession` until it ends. Its
lifecycle, all in the worker (`internal/worker/worker.go`, `serveAttach`):

1. **Attach.** The worker numbers it `<kind>-<n>` (`attach-3`, `tui-1`), where `n` comes from
   a block of 16 numbers the worker reserves in the session's row in `state.db` when it starts and
   whenever the block runs out, so an attach rarely waits on a database write. Ids are never
   reused within an incarnation, not even by a later worker for the same session, though a worker
   that stops (or hands off) before using its block leaves a gap. (Reuse would let a client that
   names an old id, to replace it or `agt detach --attach` it, hit someone else's attachment.) On the owner goroutine it then applies
   the client's size if it is to own the size (below), takes the snapshot (laid out for the session's size), subscribes the attachment to
   output and lists it, in one step, so the snapshot is an exact boundary: the output that follows
   continues it. If the output so far ends inside an escape sequence, that step waits for the rest
   of it (at most 100 ms; see "libghostty-vt"). `Attached`
   carries the id and the snapshot (and with `session-uid`, the UID).
2. **Live.** Output flows to the client through its bounded queue (below); its input, resizes and
   snapshot requests flow back. Several attachments may be live at once; input is shared, and only
   the size owner's resizes reach the PTY.
3. **End.** Any of: the client half-closes the stream (detach) or its stream fails (disconnect, a
   dead peer noticed after the 15-second timeout, a write that fails); `DetachAttachment` or
   `DetachSession` names it; a later attach replaces it; the session ends (`SessionEnded` follows
   the last output); the agent stops reading input and the input queue overflows (`Error`). The
   attachment is unsubscribed and unlisted on the owner goroutine as its handler returns. A handler
   blocked writing to a client that stopped reading is given 2 seconds to finish once it is told to
   end, then its write is failed.

The session never depends on any of this: it runs with no attachments at all.

A reattach after a lost connection is a new attachment with a new id; nothing of the old one is
resumed, and the screen is restored from the new snapshot. It carries two things from the old one.
`ExpectUID` makes the daemon (checking `state.db`) and the worker (checking its own UID) refuse the
attach if the name now belongs to another incarnation: the CLI reports that the session was
replaced and stops, rather than attaching the user's terminal and keystrokes to a session they never
saw. `Replaces` names the old attachment, which the worker drops at once (it is unlisted and stops
receiving output immediately; its handler ends within the 2-second grace) instead of keeping it
until its connection is noticed dead. A replace is honoured only together with a matching
`ExpectUID`, since ids are only unique within an incarnation.

### Size ownership

A PTY has one size, and every attached terminal gets output laid out for it. One attachment, the
**size owner**, decides it (`internal/worker/size.go`); the others are **pending**: the worker
records the size each reports (at attach and in `AttachResize`) but does not apply it, so a second
viewer, a phone, say, never reflows the agent's screen for everyone by attaching.

* The first attachment with a size owns it. An attachment that reports no size (zero rows or
  columns, like `agtd bench`'s) never does.
* When the owner leaves (a detach or switch, which the CLI announces with `AttachLeave` before
  closing its stream; `DetachAttachment`; closing its terminal, since the CLI detaches on SIGHUP
  and SIGTERM), the pending attachment used most recently (its last input, else its attach) takes
  over at once, and the session takes its size. With none left, nobody owns the size, it stays,
  and the next attachment takes it.
* A pending attachment takes the size over with `AttachSession.TakeSize` (`agt attach
  --take-size`) or `AttachTakeSize` on its stream (the overlay's Take over size).
* A reattach whose `Replaces` names the owner inherits the ownership, so a client that lost its
  connection comes back owning the size rather than finding a pending client resized it.
* An owner whose stream is cut rather than left may be about to reattach: a daemon restart cuts
  every attachment it proxies at once, a live handoff ends every attachment at once, and a remote
  client's connection can drop. So when the owner's stream ends during a handoff, or ends without
  `AttachLeave` from a client that sends it, the owner's id stays reserved (across a handoff it
  travels in the handoff state): the other clients reattach as pending without resizing, and the
  owner's reattach inherits the size by `Replaces`. If the owner has not come back within 10
  seconds, the most recently used attachment takes over; so does it when an owner's CLI is killed
  outright. A client without `attach-size-owner` never sends `AttachLeave`, and its end hands the
  size on at once. A remote CLI that is about to close its connection (a detach, not a switch)
  waits, at most a second, for the worker to end the stream after its `AttachLeave`: closing a
  QUIC connection drops what has not been sent yet, and a lost `AttachLeave` would read as a cut.

With `attach-size-owner`, a client hears who owns the size: `Attached` carries the ownership, and
the worker sends `AttachSizeOwner` whenever the owner or the session's size changes. Each
attachment has a one-slot signal for that (a change merges with one not yet sent, and the owner
goroutine never blocks on it); the attachment's handler reads the current ownership when it gets
to it and sends it in order with the output. Attachment records (`agt attachments`) carry each
attachment's size and whether it owns the session's. Older clients get the same policy without
the notes, and cannot take the size over.

### Output positions

Attach streams carry no output offsets or sequence numbers, deliberately. What a terminal client
needs after any gap (a reconnect, or output dropped because it lagged) is the current screen, and
the worker supplies exactly that: a snapshot taken on the owner goroutine, where output is
published, is an exact boundary in that client's stream, with everything before it inside the
snapshot and everything after it following as output. Offsets would only help a client that wants
the missed bytes themselves, which would mean the worker retaining a replay buffer per session, and
a terminal does not need them: replaying them would only redraw what the snapshot already shows. A
consumer that wants the full transcript reads history instead.

## The Input Field And Local Echo

Over a slow link every keystroke costs a round trip before the agent's echo of it comes back.
`agt attach` therefore draws typing locally, in the agent's own input field, exactly where the
agent will draw it, and sends the keys as typed (`internal/cli/echo.go`). Nothing of agt's own
is drawn: no panel, no legend, no mode. The design, the measurements it rests on and the phases
still to come (screen frames) are in `docs/designs/smooth-remote-attach.md`.

**The field.** The worker holds the agent's screen, so it is the one that can say where typed
text lands. `AttachField` on an attach stream (`attach-field`) is answered, in order with the
output, by a `protocol.Field` found on the shadow terminal (`internal/worker/field.go`): the
insertion point (the terminal's cursor, or the one inverse-video cell a program that hides the
cursor paints as its own), the column the text starts at (after a prompt glyph such as `❯ › >
$`, or column 0 in a box between rules, or column 0 at a shell prompt), how many columns a row
holds, which way the box grows (up on the alternate screen, down on the primary), the text on
each of its rows as the program has drawn it (a faint placeholder is not text; blanks between the
text and the cursor on its row are spaces the user typed, and are kept), the pen typed text is
drawn in, and the rows around the field as VT bytes (its rules and the rows it may grow into),
so the client can grow the box the way the program would and put the rows back. The answer
waits for the PTY to go quiet for 40 ms (200 ms at most), since the client asks right after
sending keys and the program draws them a moment after reading them, and it is written after the
output queued for that client before it, so it describes the screen the client has. What the
screen cannot tell is in a small per-program profile: Claude Code leaves two columns spare at
the right, Codex and pi one, and pi keeps a space that falls on a wrap where the other two drop
it. These were measured on the three programs (the `*-field` streams in
`internal/worker/testdata/streams`, with marks for each state; `TestFieldFixtures` pins the
detector against them, so a program that changes its box fails the test and gets re-recorded).
There is no field, and keys go raw, while the program reports it is `blocked` (OSC 7501), while
something other than the agent or a shell holds the PTY's foreground (an editor takes keys as
commands), or while a menu is up (the cursor hidden or highlighted rows: nothing to type into).

**The echo.** The CLI asks for the field once output has been quiet for 150 ms and after each
batch of keys. With a field in hand, a key is drawn at once and appended to a batch that goes to
the agent after at most `[composer] flush_after` (50 ms; `0` sends each key on its own). Only
keys every editor handles the same way are predicted: printable text, Backspace, Delete, Left,
Right and the Shift-Enter and Alt-Enter newlines (measured: Ctrl-J is a newline in Claude Code
and Codex but nothing in pi, and Ctrl-Enter submits in Claude Code, is a newline in Codex and
nothing in pi, so neither is predicted). Over frames, Enter on a prompt is predicted as the
submit: the batch goes at once and the field is drawn empty, so a submit does not wait a round
trip to clear the box; `submits` leaves Enter unpredicted on an empty box, a slash command, an @
mention at the cursor or a backslash continuation, where the agent may do something else, and
in raw mode, where the hold would drop the agent's answer until the field was probed. A frame
that shows something other than the empty field is adopted like any unpredicted drawing. Any
other key (Tab, Esc, Ctrl-C, Up, Down, a
mouse click, ...) goes at once, with the batch before it, and the echo pauses until the field is
seen again, so the agent's own menus, completions and submit work as they always did, one round
trip late; a field asked for before such a key is stale when it arrives, so it is ignored and
asked for again (so is one asked for before a resize, whether from SIGWINCH or a new session size
in a size ownership note). Input that edits no text (a wheel scroll, mouse motion, a focus
change, a key release, the terminal's CSI reply to a query the agent sent) goes on in order
behind the batch, or at once with none, without pausing: Claude Code, Codex and pi's fullscreen
mode turn on SGR mouse reporting on the alternate screen and scroll their transcript on the wheel
while the input box stays where it is, and the worker leaves query replies to an attached client
terminal (OSC and DCS replies still decode as keys and pause the echo, though their bytes reach
the agent). A focus report that repeats the last one sent within half a second of a repaint is
held back: a repaint turns focus reporting back on (twice for a fullscreen agent), and Ghostty
answers that with the focus even when it did not change; a change always goes, and so does a
repeat at any other time. Apart from those repeats the echo never drops or rewrites a byte. A
scroll made while typed keys are unconfirmed shows with the repaint once they are (output is held
until then, below). What the decoder holds is bounded: an incomplete sequence goes as it is after
256 bytes or 50 ms with nothing following (Alt-[ in a legacy terminal is a lone ESC [), a
bracketed paste over 64 KiB, or whose end is late, is forwarded as it arrives, unpredicted, and
given up for good after another 50 ms without its end, and opening the overlay sends whatever the
decoder holds. The overlay and the inbox ignore a paste over 64 KiB. The drawing is a
prediction; the agent is the authority. The CLI keeps the states it predicted, each with its key,
and an observed field that equals one of them, text and cursor (Left and Right change only the
cursor), confirms the keys up to it, provided that key had been sent when the field was asked for
(the worker reads the field before any input sent after the request, so a later state can only
match by coincidence); the keys after it are replayed over the observed field (the agent may have
wrapped or placed the cursor differently), and a field that equals none means the agent did
something unpredicted, so its field is adopted and the predictions dropped: one visible
correction. At most 512 predictions are kept; past that the echo lets keys through raw.

**Holding and repainting.** While keys are unconfirmed the attachment's output is dropped by the
CLI (the agent's own redraw of the field would fight the drawing), and counted. The field is then
asked for 150 ms after the last flush, together with a snapshot when a repaint is owed; once the
field confirms every key, the snapshot is painted inside synchronized output (DEC 2026: one
clear and repaint that the terminal shows as a single change) and output flows again. The one
visible compromise: while keys are in flight the screen behind the drawing is still, for about
a round trip after each pause in typing. Nothing is buffered for it: dropped output is gone, and
the snapshot is the recovery, as for a lagging client. After a lost connection the CLI reattaches
as before, then asks for the field: the keys it proves missing are sent again, and keys it does
not account for are dropped, never resent on a guess. Keys still batched when the connection
went, and keys typed before the field answers (scrolls and focus reports too), wait for it and
follow the resent ones, so each goes once and in order; a second reattach before the answer keeps the resend owed, and past 64 KiB
of held keys the resend is given up and they go at once. A resize before the answer drops the
predictions it would be checked against, so keys lost with the connection are then not resent;
nor are they when the overlay opens first, or when the new worker does not report the field
(the keys still batched go at once then). The field is the only evidence (the worker does not acknowledge input):
a key that reached the PTY before the cut but that the agent had not drawn when the field was
read (the worker waits for output to settle, at most 200 ms) is sent twice.

**Growth.** When the drawing needs more rows than the agent has, the rows the box pushes move
with it (the rule above it on the alternate screen, the rule and status rows below it on the
primary one), and a box that would pass the bottom of the primary screen scrolls the terminal,
as pi's own growth does. Rows the agent will fill once it catches up (a shrinking box) are
blanked until the repaint.

**Limits.** A program that draws a right border loses it under the drawing until the repaint
(none of the three measured programs draws one). The field is detected on quiet, so a program
that moves its box while the user types is corrected at the next field. Keys typed during a
reconnect are still dropped. Frames (the next phase of the design) remove the hold and the
repaint by composing the drawing over the agent's live screen.

**Composed input.** A script, or the overlay's Submit and switch (offered with `[attach]
submit_and_switch`), has a whole prompt rather than
keystrokes, and only the worker knows how the program asked to receive input. `agt send-input
--compose` (`SendInput.Compose`) and the attach stream's `AttachCompose` (both `attach-compose`)
hand the worker the text, which it encodes on the owner goroutine from the terminal's modes
(`internal/worker/compose.go`): the text as a paste, bracketed when the program turned bracketed
paste on and otherwise with newlines as carriage returns and unsafe control bytes made spaces
(libghostty's paste rules), then Enter as the program's keyboard mode wants it (`CSI 13 u` under
the kitty protocol's report-all-keys flag, modifyOtherKeys, or a carriage return), queued as one
write. Empty text is Enter alone, which is how `Ctrl-Y`, `Enter` submits what the local echo
typed: Enter goes on the attach stream behind the keys the echo flushed when the overlay opened,
then the CLI moves to the running session that most needs attention (the list's order), or stays
when none does. `SendInput.Compose` is written only when set, since the daemon forwards the
request to the worker in the base encoding; a worker from before the capability rejects the
trailing byte rather than typing the text unencoded.

## Screen Frames

A remote attachment is a round trip away from the program, and the raw output stream serves it
poorly: every byte the program writes crosses the link, a client that cannot keep up is dropped
and resynced with a visible clear, and the local echo (above) has to hold the output back while
its keys are unconfirmed. Under `attach-frames` the worker sends the screen as frames instead
(`internal/worker/frames.go`): the rows that changed since the last frame, as the VT bytes
libghostty's formatter writes for each row, with the cursor, the terminal state the client must
mirror (modes and tab stops, keyboard state, changed colours, the cursor's shape), bells and
notifications, and, when asked, the input field. The client paints each row by absolute position
(`internal/screen`, pure Go) inside synchronized output and acknowledges the frame; the worker
builds the next one when the acknowledgement has arrived and the screen has changed since, at
least 4 ms after the last, on the owner goroutine at a sequence boundary. At most two frames are
in flight per attachment (one would make a change landing just after a frame wait a whole round
trip, and a shell echoes a typed word in several writes): a slow link gets fewer, fresher frames,
never a queue of stale output and never a drop, and the worker keeps one screen's worth of row
cache per attachment and nothing else. Two in flight allow two frames per round trip, 10 a second
at 200 ms, so a client that asks for `attach-frame-window` gets a window the worker sizes from
the minimum round trip it measures between a frame and its ack over the last 10 s: one frame per
16 ms of it, plus one, between 2 and 16 (`frameRTT` in `internal/worker/frames.go`). The minimum
and not the mean, since queueing delay raises the mean and a window grown from it would queue
more. A sized window no longer says from a frame's number which ack it was built after, so each of
its frames carries that ack (`Frame.Acked`; `Frame.AckedBy` derives it from the fixed window
otherwise), and the local echo judges frames by it. A frame waits, up to 150 ms
(`frameSyncHold`), for a program inside a synchronized-output block (mode 2026) to end it, so it
never shows half a repaint; the session's final frame does not wait. `agt
attach` asks for frames from a remote daemon and takes the raw output from the local one (`[attach]
mode`, "auto"; "frames" or "raw" force it), and `agt bench echo --mode` measures either.

A frame is one formatter run over the visible screen (19 µs for 200x50), split at the CRLFs the
formatter puts between rows; the formatter resets the pen at each row, so a row paints alone.
Rows the primary screen scrolled into history since the last frame are counted from the
scrollback's growth (or, at the scrollback's cap, found by matching the rows shifted) and sent
with their final contents (`Scrolled`, `History`): the client paints them over its top rows and
scrolls with line feeds at the bottom row, so they land in its own scrollback as the program last
wrote them. Modes travel as the formatter's prefix and the client writes only the changes (a
repeated switch to the alternate screen would clear it); a full frame follows an attach, a resize,
a switch of screens or an `AttachSnapshot`. `Attached.Snapshot` carries the scrollback rows only,
ended by line feeds that push them off a cleared screen, so the terminal's native scrollback
starts full before the first full frame. The field is read for a frame when the client asked for
it outright (`AttachField`), or when it asked in its acknowledgements (`FrameAck.Field`, while it
has an echo) and either the program has settled (`fieldSettle`) or the client typed within the last
500 ms, so keys typed during a streaming answer are still confirmed. The echo composes over frames:
while keys are unconfirmed its predicted rows are painted after each frame's rows, and the frame's
field reconciles the queue, so output is never held and the screen behind the typing keeps moving.
A frame's field is not an answer to a probe, so what it proves comes from the ack it was built
after: the worker reads requests in order, and under `attach-frame-window` each frame carries
`Acked`, the last ack the worker had read when it built it; without it the window is 2, so frame N
was built after reading the ack of frame N-2 (`protocol.FrameWindow`). Either way the frame shows
every key the client sent before that ack. Only those
keys can be confirmed by it. A field that equals only a later prediction may or may not show it
and is ignored, and so is a frame built before an unpredicted key or a resize could reach the
worker; the field is then asked for again, and each ask makes a frame, so a frame past both bounds
follows. A reattach's new frames count from 1 and show whatever reached the agent on the lost
attachment, so the first field settles the resend.
A frame client's state is not handed over: after a worker handoff or a daemon restart the client
reattaches and starts from a full frame.

**The transcript record.** Claude Code, Codex and pi in fullscreen mode repaint their transcript
on the alternate screen by absolute position, so a row that leaves the top is gone from the
terminal, and the CLI's wheel prediction needs the rows above the screen to draw a tick at once.
The worker records them (`internal/worker/altrecord.go`): after each feed on the alternate screen,
at each end of a synchronized-output block in it (a look inside one sees half a repaint), it
formats the screen and a `screen.Transcript` finds how far the content moved and appends the rows
that left, each once (docs/designs/alt-screen-scrollback.md). Where the transcript ends, above the
input box, comes from field detection, run only when the rows under the box changed and at most
every 100 ms (a detection costs 1.5 ms). Only programs whose field profile says they draw a
transcript there are recorded, decided each time a program enters the alternate screen; leaving
it ends the record. A client that asks for `attach-alt-record` (the CLI does when its painter
keeps a record: `predict_wheel` or `alt_scrollback`) gets in each alternate frame the screen's
position, the record's length and jumps, the transcript's end, and the rows it does not hold yet:
the newest 2,000 in the first frame after an attach, at most 1,000 in a later one. The record
holds at most 10,000 rows per session (about 1 MiB) and travels in the handoff state; a crash
loses it with the terminal. Against an older worker the painter infers its own record from the
frames, as before, holding only what scrolled past while it was attached. The recorded streams in `internal/worker/testdata` are
replayed frame by frame through the painter into a second libghostty terminal and compared cell
for cell (`TestFramesReplayStreams`); what differs is what the formatter cannot express (see "The
reattach snapshot").

## SSH Agent Forwarding

A program in a session (an agent running `git push`, `ssh`, or signing a commit) can use the SSH
keys of the machine the user attaches from, as `ssh -A` gives a devbox, without tying the session
to a connection (`internal/worker/sshagent.go`, `internal/cli/sshagent.go`, `internal/sshagent`;
the design and its later phases are in `docs/designs/ssh-agent-forwarding.md`). The private key
never leaves that machine: what crosses the wire is the SSH agent protocol (draft-miller-ssh-agent),
programs asking for public keys and signatures, relayed whole on a stream of its own.

Every session the daemon starts gets its own agent socket, `ssh/<uid>.sock` in the root
(`[ssh_agent] session_socket`, on by default). The worker binds it before the agent starts and
names it in the agent's `SSH_AUTH_SOCK`. The socket belongs to the session, as the PTY does, and
stays put across reattaches, daemon restarts and live handoffs; it is named by the UID, so a
process left over from an earlier incarnation of the name never reaches a later one's keys. What
answers on it does not belong to the session: each connection a program opens is routed, on its
first message, to

1. the forwarder of the attachment that owns the session's size (see "Size ownership"): taking
   over the size takes the keys along, and a pending attachment's forwarder is not used;
2. else, while the owner's id is reserved for its reattach and the owner was forwarding, or while
   `agt` opens another forwarder in place of the owner's, which ended in the last 10 seconds
   (whether or not it owned the size then), nowhere yet: the connection waits, at most 10
   seconds, for the owner's next forwarder, so a push made during a daemon restart or a dropped
   connection goes through once the client is back, and never meanwhile with the devbox's own
   keys;
3. else the agent the worker inherited (the daemon environment's `SSH_AUTH_SOCK`), relayed byte
   for byte and unfiltered, as sessions got before, unless that is a socket in the root's `ssh/`,
   another session's (a daemon started from inside a session inherits one), which is ignored and
   kept out of the agent's environment; the path is followed through symlinks, and again for each
   connection, since a shell's `ln -sf "$SSH_AUTH_SOCK" ~/.ssh/ssh_auth_sock` can point a fixed
   path at a session's socket;
4. else no agent: an empty identities answer, and `SSH_AGENT_FAILURE` to anything else, so ssh
   carries on with its own key files.

A session that requires a forwarder (a clone, `Args.RequireForwarder`) skips step 3, and fails
to start rather than fall back if its socket cannot be bound. Until 30 seconds after its start, a
connection with no forwarder to go to waits for one instead of reaching step 4, which covers the
moment between the session's creation and the client's attach and forwarder. Its gaps are
recorded as `ssh_agent_unavailable` even before anyone has forwarded.

So with the socket on, a session always has an agent at `SSH_AUTH_SOCK`, one that may hold no
keys: `ssh-add -l` exits 1 (no identities) where it used to exit 2 (no agent), and a shell setup
that starts its own agent only when none answers no longer does. While a forwarding owner holds
the size, the inherited agent is not asked at all.

A connection keeps its route until it closes, even when ownership moves, since the client's agent
keeps state per connection (the session bindings below). One that waited for an owner's reattach
is dropped if its program closed it meanwhile, so the client is never asked for a signature nobody
waits for.

A forwarder is an `ssh-agent` stream (`ForwardSSHAgent`, `internal/protocol/sshagent.go`) that
`agt` opens beside every attachment when `[ssh_agent] forward` is set: `agt attach`, `agt new`,
the inbox, the overlay's switch, and again after every reattach. It names its attachment and ends
with it. It opens a moment after the attachment, since attaching does not wait for it, and a connection made
in that moment (outside a reattach, where the wait above covers it) gets the fallback. The daemon
checks the incarnation (`ExpectUID`) and that the worker has a socket, sets `Client` to who the
connection is (the client key's name and fingerprint, or `local`), and then copies bytes as for an
attach. The worker sends each program's messages as `SSHAgentRequest` frames tagged with the
connection's number; `agt` answers each with `SSHAgentReply` from its own agent (`[ssh_agent]
agent`, else `$SSH_AUTH_SOCK`), holding one connection to it per program connection, and
`SSHAgentClose` from either side ends one. `agt` decides what passes: only listing keys, signing,
and the `session-bind@openssh.com` and `query` extensions reach the user's agent, so nothing on
the devbox can add, remove or lock keys, and `[ssh_agent] keys` limits the keys offered and used.
The worker refuses the rest too, to spare the round trip. For a session on another host, `agt`
first binds each connection to its agent to the agtd hop, as a forwarding session to a host key it
makes for that forwarder (`session-bind@openssh.com`, as `ssh -A` binds what it forwards), so
OpenSSH's destination constraints apply: a key added with `ssh-add -h` is hidden and refused on the
devbox. Until the agent has taken that binding (a locked agent takes none), it is tried again
before each request and after each listing, which is asked again once it holds, since an agent
unlocked in between lists every key, and the program's own bindings are refused. `agt` opens a
forwarder only when its agent, asked the same way, answers and holds a key `keys` allows, since a
forwarder that can answer nothing would hide the devbox's own agent. It opens one again, with a
wait that grows from 1 to 8 s (under the worker's 10 s wait for it, and starting over only after a
stream has served 8 s), when one ends while its attachment lives; three refusals in a row from the
daemon or the worker are final.

Signatures, refusals, and a program finding no agent (once per gap in forwarding, and only in a
session that has been forwarded to) are info events, `ssh_agent_signed`, `ssh_agent_refused` and
`ssh_agent_unavailable`, naming the key, the client and what was signed: a login, as whom, and to
which host when the login's session is one the client's agent accepted a binding for (OpenSSH's
checks the binding's signature; the host key is named from the devbox's `known_hosts` when it can
be), or an SSHSIG namespace such as git's. Every signature is recorded, and the session's end waits
up to a second for one given just before it. Refusals are paced: a refusal starts a quiet time of
10 seconds, and those like it within it are held back; when it ends an event counts them, and if
there were any, another quiet time follows, twice as long, up to an hour; if not, the refusal is
forgotten. While 16 different refusals are in their quiet time, any other is held back in one
shared quiet time on the same doubling clock, and while that holds any back, new refusals join it.
So a program looping on a refused request writes about ten events in its first hour and one an
hour after, and one varying what it asks 16 more; a session keeps its newest 500 events. The
session's end or a handoff writes the counts at once. Refusals after the session's end are not
recorded, and counts still queued when a handoff execs the new binary are lost. They never raise
attention.

The exposure is that of `ssh -A`: while a forwarding attachment owns a session's size, any process
running as the daemon's user on that host can ask for signatures (agents are not sandboxed, and
every session's socket is reachable), and it ends with the attachment. Checking where a key is used
(only github.com, say) and asking the user are the design's next phase.

## Duplicate And Replayed Requests

A client whose connection dies after it sent a request cannot know whether the daemon acted on it.
Reads are simply asked again. Requests with side effects carry a client-chosen token
(`request-tokens`): `CreateSession`, `KillSession` (stop and `rm`), `AddWorkspace` and
`RemoveWorkspace`. The CLI generates one random token per command and, if the connection to a
remote daemon is lost after sending, sends the same request with the same token on a new
connection. The daemon answers a token it has seen with the original answer: a retried `new`
returns the session it created (waiting for it to finish starting if need be), not "already
exists"; a retried `kill` or `rm` returns what the first one did, not "not running" or "not found".
A duplicate that arrives while the first is still running waits for it. A token reused for a
different request is refused; a kill's `ExpectUID` counts as part of the request here whatever the
connection negotiated, so one token never stands for kills of two incarnations.

`CreateSession`'s token is stored on the session's row (`sessions.create_token`, unique), so a
retry finds the session even across a daemon restart. The others are remembered in memory for 10
minutes, at most 4096 of them (`internal/daemon/replay.go`); after a daemon restart a retried
`kill` or `rm` reports what it finds, which is the state the client asked for. `send-input` and
`detach` carry no token and are never resent.

## Slow Clients, Buffering And Backpressure

The PTY is never blocked by a client, and nothing buffers without bound. Every stage on the path
between the PTY and a client either blocks its producer (backpressure) or, at exactly one place,
drops with a defined recovery.

Output, per attachment, from the PTY to the screen:

| Stage | Bound | When full |
|---|---|---|
| PTY reads (worker) | batched into chunks of up to 8 KiB, published on the owner goroutine | a full batch stops the PTY reader until the owner takes it; publish never blocks |
| Subscriber queue (worker, `broadcast.go`) | 1024 chunks and 1 MiB, whichever first | the chunk is dropped and the attachment marked lagged |
| Attach handler (worker) | one frame being written: 8 KiB of output, or one snapshot | blocks: the queue above fills |
| Worker to daemon Unix socket | kernel socket buffers (8 KiB each way on macOS, about 200 KiB on Linux) | blocks the handler |
| Daemon proxy (`proxy.go`) | one 32 KiB copy buffer | blocks: stops reading the worker |
| QUIC stream (remote only) | the client's stream receive window (`quicConfig`): 512 KiB, grown towards 4 MiB only for a reader that keeps up; 16 MiB per connection | blocks the daemon's copy for this stream only |
| CLI frame queue (`attach.go`) | 16 decoded frames plus a 4 KiB read buffer | stops reading: the window above fills |
| Terminal | the CLI writes to stdout synchronously | stops the CLI's loop: the queue above fills |

So a remote client that stops reading holds at most about 1 MiB plus one snapshot in the worker,
32 KiB plus QUIC's unacknowledged data (at most its window) in the daemon, and the window plus 16
frames in the CLI. The rest of its output is dropped in the worker. Over QUIC each attachment is its
own stream with its own flow control, so the stall stops at that stream: other attachments and
requests on the same connection, and other clients, keep flowing. Over the Unix socket the same
holds per connection.

Recovery from the drop is a resync. The client asks for it in its attach feature list
(`attach-resync`). Once its queue overflows, the worker sends the next thing it can, at the moment
the client has drained what was in flight (the blocked write went through), as an `AttachResync`:
a fresh snapshot taken on the owner goroutine, discarding what was queued, which is an exact
boundary like any other snapshot. The CLI clears the screen and paints it, so the screen is correct
again however much was dropped; with the overlay open it ignores it, since closing the overlay
repaints from a snapshot anyway. If output keeps arriving faster than the client takes it, it gets
a resync each time it drains, which keeps it as current as its link allows. A frame client
(`attach-frames`, see "Screen frames") has no queue to overflow: it is sent one frame at a time
and the next is built from the screen as it is once all but the newest are acknowledged, so it
falls behind by at most its window (2, or up to 16 under `attach-frame-window`) and is never
dropped or resynced; one that stops reading
altogether leaves the worker blocked writing a frame to it with nothing queued behind, and is
released when its connection goes (`TestStalledFrameClientIsBounded`: 67 MB through the PTY with
the worker's heap flat). A raw client that stalls
outright may do so with a resync already in its blocked write; when it reads again it gets that
stale one first and, since it lagged meanwhile, a fresh one right after. A client that did not
ask for resyncs (an older CLI) keeps the old behaviour: its screen is wrong until the program
repaints or it reattaches. A resync carries only the screen when the attach capped its scrollback
(`attach-scrollback`), a few KiB; otherwise it carries the retained scrollback too (scrollback
memory is capped at 10 MB), typically some hundreds of KiB.

Input goes the other way: the CLI's stdin reader queues up to 32 reads of 4 KiB; the CLI writes
input frames to the stream as the user types, blocking when the stream's window is full; the
daemon's proxy copies through one 32 KiB buffer; the worker's attach handler reads one request at
a time and hands input to one bounded queue per session (256 writes and 4 MiB), drained by a
dedicated writer. An agent that stops reading its input therefore never stalls PTY output or other
requests. Once the queue is full, further input is refused with an error until the agent catches
up: `send-input` gets the error, and an attachment ends with it rather than silently dropping
keystrokes.

SSH agent forwarding (see "SSH Agent Forwarding") is bounded at every stage:

| Stage | Bound | When exceeded |
|---|---|---|
| One agent message, either way | 256 KiB, OpenSSH's own limit | the connection is closed |
| A frame on an open `ssh-agent` stream, either way | one message and its fields (`MaxSSHAgentFramePayload`), checked at the frame's header | the stream ends |
| Program connections per session (worker) | 64 open at once | further ones are closed as they arrive, and logged at most once a minute; ssh carries on without an agent |
| Waiting for a program's request (worker; the first too, so ssh-add can ask for a passphrase; not on a relay to the inherited agent after its first) | 15 min | the connection is closed |
| Requests in flight per program connection | 1: the worker reads the next request only after writing the reply | n/a |
| Session bindings remembered per program connection (worker) | 16, OpenSSH's own limit | later ones are not remembered, and a login they bound is recorded without its host |
| Forwarders | one per attachment, ending with it | a second is refused, unless the first has ended |
| Waiting for the owner's next forwarder: its reattach, or another in place of one that ended | 10 s from the owner's cut or its forwarder's end | the connection is routed as ownership then stands, or dropped if the program has gone |
| Waiting for a client's reply (worker) | 90 s, room for a confirmation the user's agent asks for over a slow link | `SSH_AGENT_FAILURE` to the program, `SSHAgentClose` to the client, and the connection is closed |
| A write to a forwarder (worker) | 10 s | the forwarder is dropped and its requests fail; agt opens another |
| A reply to a program (worker) | 10 s | the connection is closed, so a program that never reads its answers cannot hold a place |
| Dialling the inherited agent (worker) | 2 s | the program gets no agent: no keys, and `SSH_AGENT_FAILURE` to anything else |
| A connection relayed to the inherited agent (worker) | none past its first message: it is relayed byte for byte, as before | n/a |
| Refusal events (worker) | each different one once per quiet time, 10 s doubling to 1 h while it repeats, 16 different ones at most; every signature is recorded | held back, and counted in an event when its quiet time is up |
| Waiting at the session's end for a signature's event (worker) | 1 s | the event is not recorded |
| Local agent connections (CLI) | 64, one per program connection, each counted until its goroutine ends, dialled within 2 s; the hop's binding, like a request, waits up to 90 s for an agent busy confirming another, and ending the forwarder ends the wait | `SSHAgentClose` |
| A write to the worker (CLI) | 10 s | the stream is closed, and agt opens another |

So a forwarder can make the worker hold at most 64 messages of 256 KiB, and in practice a few
hundred bytes each. A client that stops reading stalls only the program connections routed to it,
and only until the write deadline, never the PTY, attachments or other forwarders, since each
forwarder is its own stream.

## Multiple Hosts

Each host's daemon owns its own sessions; there is no directory of sessions anywhere else. The CLI
makes several hosts feel like one workspace by asking each in turn (`internal/cli/hosts.go`,
`allhosts.go`): `agt hosts` probes this machine's daemon and every host in `hosts.toml` in
parallel within four seconds; `agt ls --all` merges their session lists into one, ordered by
what needs attention first, with a HOST column; `agt events --all` follows every host's event
stream on its own connection, each resuming after its own last event. An unreachable host is
reported and skipped, never waited on.

A session's global identifier is its daemon's id and its UID (`protocol.GlobalSessionID`). The
daemon's id is the fingerprint of its key (`remote/daemon.key`, created at startup if missing),
which is also what remote clients pin, so a host has one id however clients name it. The UID
changes with every incarnation of a name. `host/name` stays the address people type; the global id
is what scripts can keep (`agt ls --json`, `agt status`).

`agt --host auto new` is a placement hint, not a scheduler: among hosts that are online, have the
agent and (with `--workspace`) the workspace, it picks the fewest running sessions per CPU.

### The inbox

Bare `agt` is the inbox (`internal/cli/inbox.go`): every session on this machine and on every host
in `hosts.toml`, in one list drawn below the prompt, the ones that need you first. Rows are keyed by
host and session UID, so the same name on two hosts is two rows and the selection follows its
session through reorders. `agt --host NAME` shows that host alone and never starts or asks this
machine's daemon; otherwise the inbox starts the local daemon as any command does, showing
`local ◌ starting` until it answers.

Hosts reach the inbox through a `hostSet` (`hosts.go`). One goroutine per host owns that host's
client, keeping its connection and control stream open between probes, and probes it one probe at
a time: every 200 ms for this machine's daemon, every 2 s for a remote one, and for a host that
cannot be reached after a backoff from 2 s doubling to 30 s, which Ctrl-R resets. Each probe goes to
the inbox as a new `hostProbe` the goroutine never touches again, on a channel that holds one per
host, so a busy inbox only delays probing and nothing is shared. Stopping the set cancels every
goroutine, closing its connections, and waits for them; the inbox stops it before attaching, so a
probe never competes with an attachment.

Actions never run on the inbox's goroutine. A remove (Tab, remove, confirmed on the line) runs on
a goroutine of its own over a client of its own for the row's host, sends `KillSession` with
`Remove` and the row's UID as `ExpectUID` (`kill-expect-uid`), and reports on a channel; the row
reads `removing…` meanwhile and refuses a second action. A daemon without `kill-expect-uid` would
remove by name, so the inbox first asks it for the session and refuses if its UID changed; a session
replaced between that answer and the kill is still removed there. Creating a session runs the same
way, and attaches to it when it is up. It creates on this machine, in the current directory; in a
view locked to a remote host, Ctrl-N and the `+` row show the `agt new -C DIR host/NAME` to run instead.

What the inbox tells the user is in two kinds. A notice (a success, or the answer to a key) passes
after 10 s or at the next key. A failure (a refusal, or a lost answer, which says the result is
unknown and asks that host again rather than guessing; an answer counts as lost only once the
request was written; a local daemon that could not be started is one too) stays on screen until
a key the user can see that comes at least 500 ms after it was first drawn on screen (a terminal
too short for it does not count; a key already on its way does not clear it), and while any is unseen the inbox never leaves or attaches by itself. Quitting or
attaching while an action runs waits for every action, saying what it waits for on a line of its
own, and a second Esc leaves at once; a failure meanwhile cancels the leave, saying so when it was
an attach. Leaving prints the failures the user has not seen and any action a second Esc
abandoned in flight: at once when quitting, after the attachment ends when attaching (its first
repaint would clear them). A row's menu closes when its actions change (its session exited, say), so
the highlight never slides onto remove, and when the filter no longer shows the row. A remove
confirmation takes Enter or y only once it has been on screen for 500 ms, so keys that arrive
with or just after the one that opened it (key repeat, a burst queued over ssh) cannot confirm it
unseen; while a terminal too short for it crops it, Enter says so. A menu is checked against the
filter on every redraw, since time alone can change what it matches. Only a terminal that cannot be read or drawn ends the
inbox.

While the inbox is not on screen (before its first draw, or narrowed below 40 columns) keys and
pastes do nothing, and acknowledge nothing, but Esc and Ctrl-C quit; the narrow notice says when an
action failed or what a leave waits for. Pastes are bracketed (mode 2004) while the inbox runs, so a
pasted Tab or line break is text in the filter. The key decoder holds a lone ESC for 50 ms in case
it starts a sequence split across reads (a paste's start marker); a read that does not continue a
sequence makes it Esc at once, and a single printable byte joins it as Alt and that key. A paste
whose end does not come within 2 s of its start is given up, and the inbox then drops what follows
up to the paste's end marker, until input is quiet for 2 s (10 s at most), so the rest of a stalled
paste never acts as keys. (The overlay's decoder does not give up a paste.)

The block is redrawn by moving the cursor up over the previous drawing, so it is capped at the
terminal's height less one line (the list scrolls to keep the selection in view), every line is one
cell narrower than the terminal so none wraps, and widths are measured in terminal cells
(`uniseg`), so a wide character in a status message cannot wrap a row. Started below 40 columns, the
inbox prints its own rows as `agt ls --all` does and exits; narrowed below 40 once open, it says so
on one line until the terminal widens, and actions in flight still report.

An attachment from the inbox runs inside it (`attachFromInbox`): the client is marked `fromInbox`,
so attach enters the alternate screen itself, prints nothing ("attached to", the session's end),
and on return pops its keyboard flags and leaves the alternate screen without the detach clear.
The terminal restores the main screen with the inbox's block as it was, and the inbox redraws
over it, saying how the session ended or why it could not attach. Hosts go on being probed and
actions go on running meanwhile; what they report waits for the inbox. An agent that draws on the
main screen runs on the alternate one when attached from the inbox, so its output there is not in
the terminal's scrollback; plain `agt attach` is unchanged. The full actions menu (phase 2) and
later phases are in `docs/designs/agt-inbox.md`.

## Runtime Root

All state lives under a single root directory. The root is resolved in this priority order:

* AGTD_DIR => uses exact path (e.g., /custom/path)
* home => uses `~/.agtd`, on every platform
* XDG_RUNTIME_DIR => uses `{XDG_RUNTIME_DIR}/agtd`, only without a home directory
* TMPDIR => uses `{TMPDIR}/agtd-{uid}` (appends uid for multi-user safety)
* /tmp => uses `/tmp/agtd-{uid}` (default fallback, appends uid for multi-user safety)

The root is deliberately not a per-boot runtime directory by default. `XDG_RUNTIME_DIR` (typically
`/run/user/{uid}`, a tmpfs) is wiped on reboot, and on Linux also once the user's last session
ends, which would take `state.db`, session history and the daemon's remote key with it; every
remote client would then see a changed key. The runtime files in the root (the sockets, lock and
pid file) are harmless after a reboot: a starting daemon removes a stale socket while holding the
lock, and session liveness is judged by connecting to the session's socket.

The selected root contains:

* `config.toml`
* `agtd.sock`
* `agtd.lock` (held by the running daemon)
* `agtd.pid` (informational)
* `state.db` (schema v6; older versions are migrated forward, newer ones refused. Migrations
  must be additive, because session workers keep writing to the file across daemon upgrades), and
  its `state.db-wal` and `state.db-shm`: the daemon and every worker share it, so it uses
  write-ahead logging, where reads never wait for writes, and each process keeps a connection open
  rather than opening one per call. In SQLite's default journal every access waited for any write,
  sleeping in steps of up to 100 ms, which with many sessions made attaches take hundreds of
  milliseconds.
* `sessions/` (one socket per live session; a handoff's state file is created here and unlinked
  at once, living on only as a descriptor inherited by the new image)
* `ssh/` (one SSH agent socket per live session, `<uid>.sock`; a starting daemon removes those of
  incarnations that are no longer running, left by workers that were killed)
* `agtd.log` (output of a daemonized daemon)
* `remote/` (`daemon.key`, `authorized_clients`, and the CLI's `client.key`)
* `hosts.toml` (remote hosts known to the CLI, with pinned daemon keys)
* `logs/` (session history and `<id>.worker.log`)

The root and everything in it are private to the user (directories 0700, files and sockets 0600),
since logs hold full agent transcripts. `agtd` refuses a root owned by another user.

## libghostty-vt

We use `libghostty-vt` to restore the previous state of the terminal when a client re-attaches to a session.

How it works:

* user creates or re-attaches to a session with `agt attach <name>` or by focusing it in the TUI
* user interacts with terminal stdin
* stdin gets sent to the PTY via the daemon and the session worker
* the worker sends PTY output to attached clients and to `ghostty-vt`
* `ghostty-vt` holds terminal state and scrollback
* user disconnects
* user re-attaches to session
* `ghostty-vt` produces a terminal snapshot that is sent to the client before live output

In this way, `ghostty-vt` doesn't sit in the middle of an active terminal session, it simply receives all the same data
the client receives so it can re-hydrate clients that connect to the session. This enables users to pick up where they
left off as if they didn't disconnect from the terminal session at all.

The worker uses `go.mitchellh.com/libghostty`, whose pinned `libghostty-vt` needs Zig 0.17 and is
built `ReleaseFast` under `.build/` by `scripts/build-libghostty.sh`. Only the worker links it:
the `agt` CLI never imports the worker (or the daemon and its SQLite store), and a test keeps it
buildable with `CGO_ENABLED=0`.

### The FFI boundary

The boundary is coarse: output goes in as whole buffers, snapshots come out whole. Nothing on the
output path crosses it per cell or per character. PTY reads can be small (on macOS, a program
that writes in small pieces, such as `yes | head`, gives mostly 30 to 130 bytes per read), so the
PTY pump batches them: reads that arrive while the owner
goroutine is busy are handed over together, up to 8 KiB, as one `VTWrite` and one chunk for each
client; an idle owner gets each read at once, so batching adds no latency. A snapshot is a few
getters, one formatter run and one render-state update. [BENCHMARKS.md](BENCHMARKS.md)
has the measurements: a `VTWrite` costs about 50 ns plus the parsing, and at 1 KiB per call the
call overhead is under 1%, while output through the worker is limited by the kernel's PTY.

### The reattach snapshot

The snapshot is VT bytes that the CLI writes to the user's terminal after clearing it, laid out
for the session's size: when the attaching client is to own the size, the worker resizes the PTY
and its terminal to it first. It is built from libghostty's VT formatter, with these additions (`terminal.go`):

* **An exact boundary.** It is taken only where the parser is between escape sequences and UTF-8
  characters (`VTGround`). If an attach arrives mid-sequence, it waits for the next PTY read and
  feeds only the bytes that finish the sequence (`VTWriteUntil`, stopping at the ground boundary)
  before taking it, at most 100 ms. Otherwise the client would get the sequence's tail as live
  output and print it.
* **The primary screen under the alternate one.** The formatter only formats the active screen,
  so while a program has the alternate screen up the worker copies the terminal (its binary
  snapshot, decoded), leaves the alternate screen in the copy, and formats the primary screen and
  scrollback from that first. The alternate screen's formatting then switches the client over with
  the mode the program used, so when the program exits, the shell and its scrollback are back. The
  scrolling region is emitted once, after the switch (it is terminal-wide), and the pen is reset
  after the switch so the alternate screen does not inherit the primary one's.
* **Only changed colors.** The formatter's palette option sets all 256 colors to libghostty's
  defaults, overwriting the user's theme; the snapshot sets only the palette entries and default
  foreground, background and cursor colors that a program changed.
* **Cursor shape.** A bar or underline cursor (DECSCUSR) is restored; a block cursor is left to
  the client's default.
* **Bounded scrollback.** An attach that lists `attach-scrollback` (`CapAttachScrollback`) caps
  the scrollback rows in its snapshot (`AttachSession.ScrollbackRows`; the CLI sends
  `[attach] scrollback_rows`, 1,000 by default). The formatter is given a selection from that
  many rows back to the end of the screen, so it reads only those pages. The snapshots that
  repaint the screen later on the same stream (an overlay closing, a resync) carry no
  scrollback, and neither does the CLI's own reattach after a lost connection or a worker
  restart. The client's terminal already holds what scrolled off before them; what scrolled off
  while it was away is in `agt history`. Without the feature (an older CLI or worker), every
  snapshot carries all of the scrollback, as before.

Tests restore snapshots into a second terminal and compare them cell by cell
(`restore_test.go`), replay recorded streams from Claude Code, Codex, a shell, Vim, Neovim,
`go test` and clang cut at many points (`streams_test.go`, `testdata/streams`), and fuzz it
(`FuzzSnapshotRestore`).

What a snapshot does not restore, because the formatter does not emit it or because a VT stream
written to someone else's terminal cannot carry it (`TestSnapshotKnownLimits` demonstrates the
formatter's):

* **Soft wraps.** Every row is written with a line break after it, so a wrapped line comes back
  as separate lines that a later resize does not reflow. A wide character that did not fit at
  the end of a row leaves a spacer head there, which keeps the pen it was written with; the
  formatter writes the character on the next row and nothing in the spacer head's place. Frames
  ("Screen frames") have the same traits, being the same formatting.
* **A hyperlink that ends in a pending wrap.** The cursor block reprints the last cell to recreate
  the pending wrap, inside a fresh OSC 8, so a link without an explicit id comes back as two links
  to the same URI, the last cell its own.
* **Text printed before grapheme clustering (mode 2027) was switched.** The mode decides how
  codepoints join into cells, cells do not record which mode made them, and the snapshot sets
  the current mode before the contents, so text printed under the other mode can split or join
  differently.
* **Scrollback on the alternate screen**, which only CSI 22 J (scroll the screen into
  scrollback) can make.
* **The saved cursor** (DECSC, mode 1048), the kitty keyboard flag stack (only the current flags
  are set), kitty graphics and sixel images.
* **The window title and working directory** (OSC 0/2 and 7), deliberately: the CLI sets its own
  title, and the directory is a path on the session's host.
* **The client's own state.** The snapshot assumes a terminal in its default state: modes are
  emitted only where they differ from the defaults, and a block cursor is not emitted. Before
  every paint (attach, reattach, resync, the overlay closing, the local echo's repaint) the CLI
  turns mouse reporting off and clears the screen, so a mouse mode the agent turned off while
  its output was held or missed does not stay on; focus reporting is left alone, since Ghostty
  reports the focus whenever it is turned on (`attachRepaintSequence`). It resets the common
  modes when it detaches.
* **Other clients' sizes.** The snapshot fits the session's size, which the size owner set; a
  pending client at another size gets output laid out for that size (see "Size ownership").
