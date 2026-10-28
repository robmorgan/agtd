package protocol

// SSH agent forwarding (CapSSHAgent): a client lends its SSH agent to the
// sessions it drives. Every session's worker serves an agent socket, which
// the agent's SSH_AUTH_SOCK names, and passes the requests programs make on
// it to the forwarder of the attachment that owns the session's size: a
// stream the client opened with ForwardSSHAgent, on which the agent
// protocol's messages travel whole, tagged with the program connection they
// belong to. The private key never leaves the client; only requests, public
// keys and signatures cross the wire. See internal/worker/sshagent.go and
// docs/designs/ssh-agent-forwarding.md.
//
// On an ssh-agent stream the client sends ForwardSSHAgent first, and the
// worker answers SSHAgentForwarding (or Error). From then on the worker
// sends SSHAgentRequest, one agent message from a program, and SSHAgentClose
// when a program closes its connection; the client answers each request
// with SSHAgentReply, and sends SSHAgentClose when its own agent connection
// for a program connection is gone. Either side ends the stream; it also
// ends with the attachment it belongs to.

import "io"

// MaxSSHAgentFramePayload bounds a frame on an open ssh-agent stream: one
// agent message (at most sshagent.MaxMessageLen, 256 KiB) and the fields
// around it, so neither end allocates more for a frame than a message
// needs.
const MaxSSHAgentFramePayload = 256<<10 + 64

// ReadSSHAgentRequest reads a frame the client sends on an open ssh-agent
// stream, refusing one over MaxSSHAgentFramePayload before allocating it.
// It returns (nil, nil) when the peer closed the stream cleanly.
func ReadSSHAgentRequest(r io.Reader) (*Request, error) {
	return readRequestLimit(r, Features{}, MaxSSHAgentFramePayload)
}

// ReadSSHAgentResponse reads a frame the worker sends on an open ssh-agent
// stream, refusing one over MaxSSHAgentFramePayload before allocating it.
// It returns (nil, nil) when the peer closed the stream cleanly.
func ReadSSHAgentResponse(r io.Reader) (*Response, error) {
	return readResponseLimit(r, Features{}, MaxSSHAgentFramePayload)
}

// ForwardSSHAgent opens an ssh-agent stream for one attachment.
type ForwardSSHAgent struct {
	SessionID string
	// ExpectUID refuses the stream unless the session's current
	// incarnation has this UID, as for an attach. Empty means any.
	ExpectUID string
	// Attachment is the attach id this forwarder accompanies, a live
	// attachment of the session's incarnation: requests are routed to it
	// while it owns the session's size, and the forwarder ends when it
	// does.
	Attachment string
	// Client names the client for the session's events: the daemon sets
	// it to the connection's key (its name and fingerprint) or "local",
	// replacing whatever the client sent.
	Client string
	// Features are the forwarding features the client asks for, for the
	// stream to grow without a new role. None are defined yet.
	Features []string
}

// SSHAgentForwarding accepts an ssh-agent stream.
type SSHAgentForwarding struct {
	// Features are the forwarding features in effect.
	Features []string
}

// SSHAgentMessage is one SSH agent message, its type and contents without
// the length prefix (at most sshagent.MaxMessageLen bytes), for program
// connection Conn: a request from a program (SSHAgentRequest) or the
// client agent's reply (SSHAgentReply).
type SSHAgentMessage struct {
	Conn uint32
	Data []byte
}

// SSHAgentConn names a program connection on an ssh-agent stream, for
// SSHAgentClose: from the worker, the program closed it; from the client,
// its own agent connection for it is gone, or it refused to open one.
type SSHAgentConn struct {
	Conn uint32
}
