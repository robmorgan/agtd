# Design: forwarding the user's SSH agent into sessions

Status: revision 3 (2026-10-25); Phase 1 built the same day (see "Phase 1 as built"). Revision 2
settled which forwarder serves, the level of the unavailable event, and the default. Revision 3
turns forwarding on in config.toml, for every attachment, in place of the `-A` flag (see "Settled
in review").
Scope: let a program in a session (an agent running `git push`, `ssh`, or signing a commit) use the
SSH keys of the machine the user attaches from, over agtd's own connection, as `ssh -A` does,
without tying the session to that connection.

## The problem

The target user runs agents on a devbox over ssh today and forwards their agent with `ssh -A`, so
`git push` on the devbox signs with the key on their laptop, often a key that cannot leave it
(1Password, a YubiKey, the Secure Enclave). Moving to `agt --host devbox attach` loses that. A
session's agent inherits the daemon's environment (the daemon passes `os.Environ()` to the
worker, the worker to the agent: `internal/daemon/sessions.go`, `internal/worker/worker.go`), so
its `SSH_AUTH_SOCK` is whatever the daemon started with. On a devbox that is one of:

* Nothing, when the systemd unit started the daemon at boot. Pushes fail.
* The forwarded socket of whichever `ssh -A` login happened to start or upgrade the daemon
  (`install.sh` upgrades over ssh, and `agt` starts a daemon when it needs one). Every session
  created afterwards inherits that path. It works until that login ends, then fails for good with
  nothing to say why.

Copying `ssh -A` literally does not fit either. ssh ties a forwarded agent to one connection, and
agtd makes sessions outlive connections (ARCHITECTURE.md, "Sessions Belong To The Daemon"). An
`SSH_AUTH_SOCK` captured at spawn would point at a connection that is gone the first time the user
closes the laptop.

## What is forwarded, and what never is

The private key never leaves the client machine. Agent forwarding carries the SSH agent protocol
(draft-miller-ssh-agent): a program asks the agent for its public keys, then asks it to sign some
bytes with one of them, and gets back a signature. Over `ssh -A` those messages ride inside the SSH
connection. Here they ride on a QUIC stream, or on the local Unix socket. The QUIC stream is already
encrypted and authenticated both ways with pinned keys, and the agent protocol does not care which
transport carries it.

So a forwarded agent gives away the *use* of the keys, never the keys themselves. While forwarding
is on, a process on the devbox that can reach the socket can ask for signatures. Once forwarding
ends it can ask for nothing. That use is what this design limits and records (see "Security").

## The model

Every session gets its own agent socket, owned by its worker, and `SSH_AUTH_SOCK` points at it for
the agent's whole life. The socket is durable and belongs to the session. Whatever answers on it is
not durable.

Once forwarding is set up in the CLI's config, every attachment it opens adds a **forwarder**: a
stream from the CLI on which the worker passes agent requests to the CLI. The CLI relays them to
the agent on its own machine once its policy has looked at them. The keys used are those of the attachment in charge of the session:
requests go to the forwarder of the attachment that owns the session's size, under the size
ownership rules (ARCHITECTURE.md, "Size ownership"). When that attachment does not forward, or
nobody owns the size, the socket falls back to the agent the session inherited, which is today's
behaviour. When there is no inherited agent either, the socket answers with no keys at once and
records why.

```text
 laptop                                      devbox
 ──────                                      ──────
 ssh-agent, 1Password, a YubiKey             program in session foo: git push → ssh
      ▲                                              │  SSH_AUTH_SOCK=~/.agtd/ssh/<uid>.sock
      │ the CLI's own $SSH_AUTH_SOCK                 ▼
 agt attach foo (forward = true)             worker (foo), the socket's router:
   policy: which messages, which keys,         1. the size owner's forwarder ◄───────┐
   which hosts; asks the user                  2. else the agent it inherited        │
      │                                        3. else no keys, and an event         │
      │      one QUIC stream, role ssh-agent                                         │
      └──────────────────────────────────► agtd: proxied raw, like an attach ────────┘
```

## Decisions

1. **The socket belongs to the session's worker.** It is bound at `<root>/ssh/<uid>.sock` before
   the agent is spawned, and `SSH_AUTH_SOCK` names it in the agent's environment. It moves across a
   live handoff as the session socket does: its descriptor is inherited and listed in
   `handoffState`. It goes away when the worker exits. The worker has exactly the session's
   lifetime, survives daemon restarts, and is already where per-session state lives. "The daemon
   holds no session state that is not in `state.db` or in a worker" stays true: the daemon proxies
   the forwarding stream the way it proxies attach streams.

   The path uses the UID rather than the name because a name is reused by later incarnations. An
   orphan of an earlier incarnation (an agent that ignored SIGHUP) must not reach a new session's
   forwarders. The path is also a fixed root + 42 bytes, which fits any root that can hold a session
   socket for a 64-character name. If the socket cannot be bound (a root longer than about 60
   bytes), the session starts as today with the inherited `SSH_AUTH_SOCK`, and its worker does not
   offer forwarding. A starting daemon removes `ssh/*.sock` files whose UID is no longer a running
   session (left behind by a worker that was killed).

2. **Forwarding is a client setting that covers every attachment.** It is off until
   `[ssh_agent] forward = true` is set in the CLI's `config.toml` (`~/.agtd/config.toml` on the
   machine `agt` runs on). From then on every attachment that CLI opens forwards, whichever command
   opened it: `agt attach`, `agt new` (which attaches after creating), the inbox (bare `agt`), the
   overlay's switch, and submit and switch. There is no flag either way: forwarding is something
   a user sets up once, not something to remember on every attach. The CLI opens the forwarder
   in its attach loop
   (`attachOnce` in `internal/cli/attach.go`, once for each attachment it makes, reattaches
   included), which all of those go through. A forwarder lives as long as the attached `agt`
   process:
   * The CLI reopens it after every reattach (a lost connection, a daemon restart, a worker
     handoff).
   * It moves with the overlay's session switch.
   * It ends on detach.

   Local attachments forward too. With the setting in the devbox's own config, `agt attach` there
   after `ssh -A devbox` forwards that login's agent into the session. That keeps the target user's
   current workflow working and fixes the stale socket described above.

3. **Routing: the size owner's forwarder, then the inherited agent, then nothing.** The forwarder
   that serves follows size ownership exactly and keeps no ownership state of its own. The worker
   reads the owner on its owner goroutine, as attach handlers do. So:
   * The first attachment with a size owns it, and its forwarder serves. Taking the size over
     (`agt attach --take-size`, or the overlay's Take over size) takes the agent with it.
   * When the owner leaves, the pending attachment used most recently takes over, and its
     forwarder serves.
   * A forwarding attachment that does not own the size is not used. To use its keys, take over
     the size.
   * While the owner's id is reserved for its reattach (after a lost connection, a daemon restart
     or a handoff), and the owner was forwarding when its stream was cut, a new connection waits
     up to 10 seconds for the reattached owner's forwarder. It does not fall through to another
     agent while it waits. After that it goes wherever ownership has settled.

   Each connection a program makes to the socket gets its route on its first message. It keeps
   that route until it closes, even if ownership moves meanwhile, since the client's agent keeps
   state per connection (the session bindings in "Security").
   * The inherited agent is the `SSH_AUTH_SOCK` the worker started with. It is relayed byte for
     byte with no filtering. The agent could already reach it directly, so nothing is exposed that
     was not before, and `ssh-add` against it works as it does today.
   * With neither, the socket returns an empty identities answer, and `SSH_AGENT_FAILURE` to
     anything else. ssh then carries on with key files on disk, as it would with no agent at all.

4. **The policy runs on the client.** The machine whose keys are used is the trust anchor. The
   devbox, which runs unsandboxed agents, is what the policy guards against. The worker filters
   requests as well, but only to save a round trip. The CLI makes the decisions.

5. **One stream per forwarder, with the program's connections multiplexed on it by id.** Agent
   messages are a few hundred bytes, so head-of-line blocking between them costs nothing. A stream
   per program connection would need the daemon to ask the client to open streams: QUIC streams
   are opened by the client here, and a Unix connection is a single stream. Messages are relayed
   whole and verbatim, never re-encoded, so an extension the policy allows passes through
   unchanged.

6. **Fail fast and record it.** Apart from the wait for the owner's reattach above, a request that
   finds nobody to answer it is not held, because whoever is waiting on ssh would give up first:
   an agent's tool call has its own timeout, and the server has its login grace time. Instead the
   worker records `ssh_agent_unavailable`, an info event that never raises the session's
   attention. The agent sees its command fail and handles that as it handles any failure; the
   event only records why. It is recorded at most once per gap in forwarding, and only for a
   session that has been forwarded to before, so a session that never asked for forwarding never
   reports a missing agent.

## Protocol additions (under one capability; golden frames and fuzz for each)

Capability `ssh-agent`: the daemon lists it in `Welcome` when it can proxy forwarding streams. A
worker lists it in its answer to `Hello` only if its agent was given the socket. A session created
before this feature cannot forward even after a handoff to a new binary, because its agent's
environment is fixed.

A new stream role, `ssh-agent`, opened by `ForwardSSHAgent`:

| Role | First frame | Then |
|---|---|---|
| ssh-agent | `ForwardSSHAgent` | `SSHAgentForwarding` (or `Error`). Then `SSHAgentRequest` and `SSHAgentClose` from the worker, and `SSHAgentReply` and `SSHAgentClose` from the client, until either side ends the stream |

| Message | Kind | Fields |
|---|---|---|
| `ForwardSSHAgent` | 80 | `SessionID`, `ExpectUID` (refused if the name now belongs to another incarnation, as for an attach), `Attachment` (the attach id it accompanies, which must be a live attachment of that incarnation: routing follows that attachment, and the forwarder ends when the attachment does), `Client` (set by the daemon, see below), and a trailing feature list as in `AttachSession`, so the stream can grow without a new role |
| `SSHAgentReply` | 81 | `Conn u32`, `Data`: the client's agent's reply, one agent message without its length prefix |
| `SSHAgentClose` | 82 (client), 182 (worker) | `Conn u32`. From the worker: the program closed its connection. From the client: its agent went away, or it refused to open one |
| `SSHAgentForwarding` | 180 | the feature list in effect |
| `SSHAgentRequest` | 181 | `Conn u32`, `Data`: one agent message from a program |

Kinds 80-89 and 180-189 are the next free area. The daemon checks `ExpectUID` against `state.db` and
checks the worker's capability, exactly as `negotiateAttach` does. It then writes the request to the
worker with `Client` replaced by the connection's identity: the client key's name and fingerprint
(`transport.DescribeClient`), or `local`. A QUIC stream does not carry its connection's key today,
so the transport gains an accessor for it. After that the daemon copies bytes both ways, as
`proxyAttach` does.

## Bounds, goroutines and failure

| Stage | Bound | When exceeded |
|---|---|---|
| One agent message, either way | 256 KiB, OpenSSH's own limit | the program's connection is closed |
| Program connections per session | 64 open at once (16 in the first build) | further ones are accepted and closed at once, and logged; ssh carries on without an agent |
| Requests in flight per program connection | 1: the protocol is request and reply, and the worker reads the next request only after writing the reply | n/a |
| Forwarders | one per attachment, ending with it | a second one for the same attachment is refused with an `Error`, unless the first has ended |
| Waiting for the owner's reattach | 10 s, the size owner's grace | the connection is routed as ownership then stands, or dropped if its program has gone |
| Waiting for a reply (worker) | 90 s, room for a 60 s confirmation over a slow link | `SSH_AGENT_FAILURE` to the program, `SSHAgentClose` to the forwarder, and the program's connection is closed |
| Writes to a forwarder (worker) | one frame at a time, under a 10 s deadline | the forwarder is dropped (its stream closed) and its requests fail |
| Local agent connections (CLI) | 64, one per program connection, each counted until it ends, dialled within 2 s | `SSHAgentClose` |

The review fixes added more: a frame bound on `ssh-agent` streams, deadlines on a program
connection's requests and on replies to it, and a hold-back on repeated events. ARCHITECTURE.md's
table lists every bound as built.

So the most a forwarder can make the worker hold is 64 messages of 256 KiB, and in practice a few
hundred bytes each. A forwarder that stops reading stalls only the program connections routed to
it, and only until its write deadline. It never stalls the PTY, attachments or other forwarders,
since each forwarder is its own stream.

Goroutines: the worker has one accept loop on the agent socket (ending with the worker), one
goroutine per program connection (at most 64) and one reader per forwarder (at most one per attachment). The CLI has
one reader for the stream and one goroutine per local agent connection (at most 64). Each ends with
its connection.

Failure:

* **A forwarder's stream ends** (detach, a lost connection noticed within 15 s, a daemon restart, a
  revoked key): its requests in flight get `SSH_AGENT_FAILURE`, and the program connections routed
  to it are closed, since their session bindings lived in the client's agent connection. New
  connections follow size ownership: they wait for the owner's reattach, or go to the next
  owner's forwarder.
* **Daemon restart**: forwarding streams end and the CLIs reopen them as they reattach. The socket
  itself keeps listening, since the worker holds it. A connection made meanwhile is routed as in
  decision 3: if the owner was forwarding, it waits up to 10 seconds for the owner's forwarder.
* **Live handoff**: the listener is inherited, so the path never stops listening. Forwarding
  streams are closed, without a frame of their own, when their attachments end with
  `SessionRestarting`, and agt opens a new forwarder when it reattaches. Program connections open
  at the exec are closed, so an ssh that is in the middle of authenticating fails once.
* **Worker crash**: the socket goes with the session.
* **An `agt attach` run inside a session** forwards that session's own socket, its
  `SSH_AUTH_SOCK`, so requests take one more hop, as `ssh -A` through two hosts does. A cycle (a
  session's socket forwarded back into the same session) ends at the connection cap, and the
  requests caught in it fail.

## Security

**Who can use a forwarded agent.** Any process running as the daemon's user, or as root, on a host
where a forwarding attachment owns a session's size, for as long as it does. That includes the
session's agent and everything it runs, and other sessions' agents, which can reach every session's
socket. With `forward = true` that means every host the user drives a session on. Agents are not
sandboxed (a known gap), so the per-session socket gives attribution and routing, not isolation.
This is the same exposure as `ForwardAgent yes` in ssh_config, and it ends the moment the
attachment does.

**What they can get.** Signatures that the client's policy allows. Never keys. No adding,
removing, locking or unlocking keys.

**The policy.** It runs in agt, on the client, in this order:

1. **Messages** (Phase 1). Only these reach the client's agent: `REQUEST_IDENTITIES` (11),
   `SIGN_REQUEST` (13), and `EXTENSION` (27) for `session-bind@openssh.com` and `query`. Anything
   else is answered with `SSH_AGENT_FAILURE` without reaching the agent. Requests to the
   inherited agent on the devbox are not filtered (decision 3).
2. **Keys** (Phase 1). `keys` lists the fingerprints offered. Others are left out of the
   identities answer and refused for signing. This also avoids servers' "too many authentication
   failures" when the agent holds many keys.
3. **Destination** (Phase 2). OpenSSH 8.9 and later bind each agent connection to the server ssh
   is authenticating to (`session-bind@openssh.com`: the server's host key, the session id, the
   host key's signature over it, and whether this hop forwards the agent further). agt verifies
   that signature. It then signs for SSH user authentication only data that names a bound session,
   since the session id is the first field of the signed data. The bound host key is looked up in
   the client's own `known_hosts`, hashed entries included, and `allow_hosts` lists the names that
   are signed for without asking. A devbox cannot forge a binding to github.com without connecting
   to github.com, so "github.com only" holds even against a compromised devbox. It limits *where*
   the key is used, not *who* on the devbox uses it.
4. **Other signatures** (Phase 2). An SSHSIG request (`ssh-keygen -Y sign`; git signs commits and
   tags under the namespace `git`) is recognised by its preamble. `allow_namespaces` lists the
   namespaces that are signed without asking.
5. **Onward forwarding** (Phase 2). A connection bound with the forwarding flag set is refused.
   That would be a program on the devbox running `ssh -A` to a third machine and offering the key
   there.
6. **Everything else asks the user.** That covers an unbound authentication (ssh older than 8.9),
   a host or namespace that is not listed, and data that is neither authentication nor SSHSIG.

**Asking never steals keystrokes.** A dialog that popped up mid-typing would take the user's next
`y` as consent. Instead the CLI rings the bell and shows a one-line notice on the bottom row, as the
reconnect status does: `ssh: foo wants to sign in to github.com as git with SHA256:…  Ctrl-Y to
review`. The overlay then lists pending requests with three choices:
* Allow once.
* Allow for this attachment, for that key and that host or namespace.
* Deny.

A request left unanswered for 60 seconds is refused. Phase 1 has no asking: it forwards what
passes the message and key filters, as `ssh -A` does.

**Audit, on the devbox.** These are session events recorded by the worker:
* `ssh_agent_signed` (info): each forwarded signature, with the key's fingerprint, the destination
  (the bound host key, named from the devbox's `known_hosts` when it can be), the user name in the
  request, and the client from `Client`.
* `ssh_agent_refused` (info): the worker or the client refused a request. Repeats are held back
  and counted in a later event (see "After review").
* `ssh_agent_unavailable` (info, never raising attention): see decision 6.

Identity listings are not recorded, and the signed data never is.

**Other notes.**
* Agents that confirm use themselves (1Password, a YubiKey's touch, keys added with `ssh-add -c`)
  keep doing so, since the signing happens on the client.
* Revoking a client key closes its connections within 5 seconds, and its forwarders with them.
* On a local attachment the policy is advisory: an agent on the same machine as the user's agent
  could reach that agent's socket itself.

## Configuration

```toml
# config.toml (the daemon and the CLI share it)
[ssh_agent]
# Daemon: give each new session its own agent socket (decision 1). Off leaves
# SSH_AUTH_SOCK as the daemon inherited it, as before this feature.
session_socket = true

# CLI: forward the agent on every attachment this CLI opens (decision 2).
# Off by default.
forward = true
# CLI: the agent forwarded (default: $SSH_AUTH_SOCK).
agent = "~/.1password/agent.sock"
# CLI: keys offered (SHA256 fingerprints); empty offers every key the agent holds.
keys = []
# CLI, Phase 2 (not yet taken: until then agt stops forwarding and says so):
# hosts signed in to without asking, matched against known_hosts.
# allow_hosts = ["github.com"]
# CLI, Phase 2: SSHSIG namespaces signed without asking ("git" signs commits).
# allow_namespaces = []
```

With `forward = true` but no agent to forward (no `SSH_AUTH_SOCK` and no `agent`), the CLI
attaches without forwarding. So it does on a session that cannot forward: one created before this
feature, or by a daemon with `session_socket = false`. Neither prints anything, since the setting
covers every attachment and a warning on each would be noise. Phase 2's `agt attachments` and
`agt status` show which attachment forwards.

## Phases

### Phase 1: the socket and plain forwarding (about 1 week)

* `internal/sshagent`, pure Go and shared by the CLI and the worker (the CLI cannot import the
  worker). It handles message framing with the 256 KiB bound, message kinds, key fingerprints, and
  the identities filter, using `golang.org/x/crypto/ssh` for keys. That is a module already in the
  build, through quic-go.
* Worker: the socket and `SSH_AUTH_SOCK`, the router with its inherited fallback, the forwarders,
  the bounds, the events, and the listener in `handoffState`.
* Daemon: `ForwardSSHAgent` proxied after its checks, with `Client` stamped, and the startup sweep
  of `ssh/`.
* CLI: `[ssh_agent] forward`, `agent` and `keys`, and the relay with the message and key filters.
  The forwarder is opened with every attachment and reopened on every reattach and switch. The
  default config.toml gains the section with its defaults (`session_socket = true`,
  `forward = false`).

The result is equivalent to `ssh -A` with a tighter message filter, and is enough for a trial.

#### Phase 1 as built (2026-10-25)

Built as above, with these additions and differences:

* **Handoff format 2.** The agent socket's listener crosses a live handoff as `SSHAgentFD` in
  `handoffState`. A binary from before this feature would resume the session and leave that
  descriptor open with nobody accepting on it, so every program asking for keys would hang. A
  session with the socket therefore writes its state as handoff format 2, which such a binary
  does not list in `--handoff-probe` and so refuses, and a session without one is still written
  as format 1. New binaries list both formats. The reserved owner programs wait for, and whether
  the session has been forwarded to, cross the handoff too.
* **The forwarder opens after its attachment.** agt opens it in the background, so attaching
  never waits a round trip for it. A connection made in that moment, outside a reattach (where
  the wait for the owner covers it), gets the fallback. In practice the forwarder's request goes
  out before anything a person types reaches the session, so only a script that runs ssh at the
  instant it attaches can see this.
* **Events.** Signatures read as `laptop (SHA256:…) signed a login as git to github.com using key
  SHA256:…`, or `… signed data in the namespace git …` for a commit signature. The destination is
  the host key of a session binding the client's agent accepted (OpenSSH's checks the host key's
  signature first) for the session the login names; any other login reads `a login as git`,
  without a host. It is for the record, and decides nothing. A host missing from the devbox's
  `known_hosts` reads `an unknown host (key SHA256:…)`, and one listed only under a hashed name (the
  default on Debian and Ubuntu) `a known host (hashed in known_hosts; key SHA256:…)`.
* **Tests as built.** `internal/sshagent` uses messages built to the protocol's layout and keys
  from `x/crypto/ssh`, with a fuzz target, rather than captured fixtures. The worker's tests use
  `x/crypto/ssh/agent` as both the program and the client's agent, and cover routing by size
  ownership, the pending forwarder, the wait for the owner's reattach and its end, the inherited
  fallback, the filters and events, the reply timeout, a forwarder lost mid-request, the
  connection bound and the message bound. The daemon's tests cover the stamped client (local and
  over QUIC), refusals, a session without a socket, the startup sweep and a real live handoff with
  a connection waiting through it. `cmd/agt` runs the real `ssh-add` and `ssh` in sessions, over
  QUIC through the relay and over the local socket, through a daemon restart, `agt new`, the
  switch to another session, and with forwarding off.
* x/crypto's agent client pipelines requests on one connection (v0.54 reads replies on a
  goroutine of its own). The worker answers a connection's requests in order, one at a time,
  which is what the protocol requires, so such clients work unchanged.
* **A remote detach now flushes its `AttachLeave`.** `agt` closes its QUIC connection as soon as
  a detach returns, and quic-go drops stream data it has not sent by then, so the worker could
  take a detach for a cut. That predates this feature (the size then waited out its 10 seconds),
  but with forwarding it kept programs waiting for an owner who had left. A remote CLI about to
  close its connection now waits, at most a second, for the worker to end the stream. The Linux
  run of the end-to-end test caught it.
* **After review (2026-10-27).** The pre-landing and adversarial reviews of PR #82 led to:
  * **The agtd hop.** A remote forwarder binds each of its connections to the local agent as a
    forwarding session first, to a host key made for that forwarder. OpenSSH's agent then applies
    destination constraints as it does for `ssh -A`: a key added with `ssh-add -h` is hidden and
    refused on the devbox. Before, the devbox's use looked local to the agent. Naming the devbox
    in a constraint is still Phase 3.
    * Until the agent takes the binding (it was locked, or rejected it), agt tries again before
      each request and refuses the program's own bindings, which would otherwise become the first
      hop and let the devbox use a key limited to another host. It tries again after each
      listing too, and asks again once the binding holds: an agent unlocked between the binding
      and the listing lists every key. The check before a forwarder opens asks the same way.
    * The binding waits its turn as a request does: OpenSSH's agent answers nothing else while it
      asks the user to confirm a signature, so a 2 s limit failed every new connection meanwhile.
  * **agt's own bounds.** A connection to the local agent counts against the cap until its
    goroutine ends, so a hostile devbox cannot grow them by closing connections as it opens
    them. An `ssh-agent` stream's frames are refused over one message at their header, before
    anything is allocated. Stopping a forwarder ends a question to the local agent in progress,
    so a detach never waits on a silent agent.
  * **The inherited agent.** An inherited `SSH_AUTH_SOCK` that leads, through any symlinks, to a
    socket in the runtime's `ssh/` is another session's socket (a daemon started from inside a
    session, or a shell's `ln -sf "$SSH_AUTH_SOCK" ~/.ssh/ssh_auth_sock`) and is ignored, checked
    again for each relayed connection.
  * **Configuration.** A mistake in `[ssh_agent]` stops agt forwarding: a setting it does not
    take (names have a case: `Keys` is not `keys`), a table spelled like it but not as it is
    (`[SSH_Agent]` would otherwise replace it), `keys` under another table, a key that is not a
    fingerprint, or a value of the wrong type (the table is decoded on its own, so the rest of
    the file stands). agt says why before an attach takes the screen and again once it gives it
    back, and the inbox shows it until a key; agtd logs it at start. agt says nothing when a
    readable `[ssh_agent]`, spelled so, sets `forward = false`. Other settings named like these
    elsewhere are ignored, as any unknown key is: misplaced, they leave forwarding as it was.
  * **Routing.**
    * A connection that waited for an owner's reattach is dropped if its program has gone,
      rather than asking the client for a signature nobody waits for.
    * While the size owner's forwarder is reopened, new connections wait for it, as for a
      reattach, rather than use the devbox's own agent, whether or not it owned the size when
      the forwarder ended.
    * A dial to the inherited agent racing the socket's stop cannot escape it.
    * A reply that races its forwarder's end, or the client's close, reaches the program before
      the connection closes.
    * A reply nobody asked for is dropped.
  * **Robustness.**
    * agt forwards only an agent that answers and holds a key `keys` allows. It opens a
      forwarder again when one ends while its attachment lives, after a wait from 1 to 8 s (under
      the worker's 10 s wait for it) that starts over only once a stream has served 8 s, and gives
      up after three refusals in a row.
    * The worker takes 64 connections, admitted before a goroutine starts for them, and logs
      those it refuses. It gives each request 15 minutes, the first too, since ssh-add connects
      before it asks for a passphrase; and it closes a connection that does not take its answers
      within 10 s.
  * **Events.** Every signature is recorded, and the session's end waits up to a second for one
    given just before it. A refusal starts a quiet time in which those like it are held back,
    10 seconds, doubling up to an hour while they keep coming; any refusal while 16 different
    ones are held shares one such quiet time. When a quiet time is up, or at the session's end
    or a handoff, an event counts those held back. Counts still queued when a handoff execs are
    lost (TODOS.md). A gap in forwarding ends
    when forwarding resumes (used or not) and is carried across a handoff. The SSH-1 request
    `ssh-add -D` sends after its own reads as such.

### Phase 2: destination policy and asking (about 1 week)

* Verifying session bindings, parsing user authentication and SSHSIG requests, matching
  `known_hosts`, `allow_hosts` and `allow_namespaces`, and refusing onward forwarding.
* The bottom-row notice and the overlay's review.
* `agt attachments`, `agt status` and the overlay's details show which attachment forwards.

### Phase 3: only if asked for

* Naming the agtd hop in OpenSSH's destination constraints (`ssh-add -h devbox>github.com`). agt
  binds each forwarded connection to a hop already (see "After review"), but to a key made for
  each forwarder, so a constrained key is refused through agtd, as through an unlisted `ssh -A`
  host. Allowing "only via devbox" needs a host key for the daemon in SSH form, published like a
  host key, and a reason not to reuse its TLS key for SSH signatures.
* A short hold for requests that find no forwarder, if agents' tool timeouts turn out to leave room
  for one.

## Tests

* `internal/sshagent`: fixtures of messages captured from OpenSSH (`ssh-add -l`, `ssh -T` against
  a test server, `ssh-keygen -Y sign`), plus fuzzing of the parser.
* Protocol: golden frames for the six message kinds, and the existing fuzz targets extended to
  them.
* Worker, with a real PTY under `-race`: a session whose program runs `ssh-add -l` against its
  `SSH_AUTH_SOCK`, with forwarders backed by `x/crypto/ssh/agent`'s in-memory keyring. It covers:
  * routing by size ownership: the first owner, takeover when the owner leaves, `--take-size`, a
    pending forwarder left unused, the wait during the owner's reserved reattach, and a
    connection keeping its route while ownership moves;
  * the inherited fallback, and the no-keys answer with its info event (once per gap, only after
    a forwarder);
  * the connection cap, the message bound and the reply timeout;
  * a forwarder lost mid-request;
  * a handoff with a request in flight: the listener is inherited and the path does not change.
* Daemon: `Client` stamped from the connection, `ExpectUID` checked, refusal for a worker without
  the capability.
* End to end (`cmd/agt`): `agt attach` with `forward = true`, over QUIC through the
  `transporttest` relay. The session's program runs the real `ssh` against an in-process
  `x/crypto/ssh` server that accepts only the client's key. It checks that:
  * the login succeeds;
  * forwarding survives an outage and reconnect;
  * the login fails cleanly, with the event, once the client has gone;
  * `agt new` and the overlay's switch forward as `agt attach` does, and nothing is forwarded
    with the setting off;
  * the same works over the local Unix socket.

  These tests are skipped where `ssh` is not installed. `make test-linux` runs them on Linux too.
* Phase 2 policy:
  * a forged binding signature is refused, and so is a session id that matches no binding;
  * hashed `known_hosts` entries match;
  * onward forwarding is refused;
  * namespaces and the key filter apply;
  * an unanswered request is refused after the timeout.

## Documentation, at each phase

* README: "Forwarding your SSH agent", covering `[ssh_agent] forward`, the policy, and what it
  does and does not protect.
* ARCHITECTURE.md:
  * the `ssh-agent` role, capability and messages;
  * rows for the bounds table in "Slow Clients, Buffering And Backpressure";
  * `ssh/` in "Runtime Root";
  * the inherited listener in "Live Handoff";
  * the new event kinds;
  * `SSH_AUTH_SOCK` in "Creating a Session".
* ROADMAP.md: the item and its phases.

## Alternatives considered

* **The daemon owns the sockets.** The daemon would have to recreate every live session's listener
  at startup, leave a gap while it restarts, and track session ends itself. That is session state
  outside `state.db` and the workers, which the daemon deliberately does not hold.
* **One daemon-wide socket, with the session found from the peer's credentials.** Mapping a pid to
  a session through reparented process trees is fragile, and it leaves no per-session fallback.
* **Forwarding bytes rather than messages.** The client could not filter, ask or audit anything.
* **Riding on the attach stream.** Signing would wait behind terminal output on a paused or lagging
  attachment. A stream of its own is the protocol's stream taxonomy: each kind of traffic has its
  own stream.
* **Holding requests until someone forwards.** Whoever waits on ssh gives up first (decision 6).
  The one wait kept is the owner's reattach, which size ownership already allows for.
* **The newest forwarder serves** (revision 1). Following size ownership instead means the
  attachment in charge of the session is the one whose keys are used, with no second ownership
  rule to learn.
* **A flag on each attachment** (`agt attach -A`, with a per-host default in `hosts.toml`;
  revisions 1 and 2). Forwarding is something a user sets up once, and a flag would have to be
  remembered on every command that attaches. A per-host setting can come back if users attach to
  hosts they trust differently. Phase 2's `allow_hosts` limits where the keys are used, not which
  devboxes may ask.
* **The socket only for sessions created with a flag.** The user would have to know at `agt new`
  that a push will come later. With the fallback, a socket in every session is transparent.

## Explicitly not in this plan

* GPG agent forwarding, and git credential (HTTPS token) forwarding. A stream like this one would
  fit them later.
* Port or X11 forwarding.
* Keys stored on the devbox or managed by agtd, or agtd acting as an agent with keys of its own.
  For work that must go on while nobody is attached, a deploy key or a token on the devbox remains
  the answer, and the README should say so.
* Sandboxing agents, which is what would turn the per-session socket from attribution into
  isolation.
* Windows agents (named pipes).
* A forwarder without an attachment (an `agt ssh-agent HOST` running in the background). It could
  never own the size, so it would never serve.

## Settled in review

On 2026-10-22:
* `ssh_agent_unavailable` is an info event and never raises the session's attention.
* Every new session gets the agtd socket by default (`session_socket = true`).
* The forwarder that serves is the size owner's, under the size ownership rules (decision 3).
* What an agent does when its ssh fails is the agent's business, and agtd adds nothing for it.

On 2026-10-25:
* Forwarding is turned on in config.toml (`[ssh_agent] forward = true`) and then applies to every
  attachment the CLI opens. There are no flags and no per-host setting (decision 2).

## Open questions

1. **Is refusing onward forwarding too strict?** An agent reaching a host through a bastion with
   `ssh -A` would need a setting to allow it, or the overlay.
2. **Is one event per signature too chatty** for an agent that fetches in a loop? If it is,
   coalesce per key, host and forwarder.
