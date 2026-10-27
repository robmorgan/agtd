// Package sshagent reads, checks and describes SSH agent protocol messages
// (draft-miller-ssh-agent) for agtd's agent forwarding. A session worker
// relays the messages programs send to their SSH_AUTH_SOCK to the client that
// drives the session, and agt checks each against its policy before it
// reaches the user's agent. Both sides use this package, and agt is pure Go,
// so this package is too.
//
// A message is a []byte holding its type and contents, without the length
// prefix. Messages are relayed whole and never re-encoded, so an extension a
// policy lets through arrives exactly as it was sent.
package sshagent

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"unicode"
	"unicode/utf8"

	"golang.org/x/crypto/ssh"
)

// Message types.
const (
	MsgFailure                    byte = 5
	MsgSuccess                    byte = 6
	MsgRequestIdentities          byte = 11
	MsgIdentitiesAnswer           byte = 12
	MsgSignRequest                byte = 13
	MsgSignResponse               byte = 14
	MsgAddIdentity                byte = 17
	MsgRemoveIdentity             byte = 18
	MsgRemoveAllIdentities        byte = 19
	MsgAddSmartcardKey            byte = 20
	MsgRemoveSmartcardKey         byte = 21
	MsgLock                       byte = 22
	MsgUnlock                     byte = 23
	MsgAddIDConstrained           byte = 25
	MsgAddSmartcardKeyConstrained byte = 26
	MsgExtension                  byte = 27
	MsgExtensionFailure           byte = 28
)

// SSH-1 requests, long obsolete. OpenSSH's `ssh-add -D` still sends
// REMOVE_ALL_RSA_IDENTITIES after REMOVE_ALL_IDENTITIES, in case the agent
// is old.
const (
	msgSSH1RequestRSAIdentities   byte = 1
	msgSSH1RSAChallenge           byte = 3
	msgSSH1AddRSAIdentity         byte = 7
	msgSSH1RemoveRSAIdentity      byte = 8
	msgSSH1RemoveAllRSAIdentities byte = 9
	msgSSH1AddRSAIDConstrained    byte = 24
)

// Extensions a forwarded agent passes on: session-bind@openssh.com, with
// which ssh (OpenSSH 8.9 and later) binds an agent connection to the server
// it authenticates to, and query, which asks which extensions an agent
// supports.
const (
	ExtSessionBind = "session-bind@openssh.com"
	ExtQuery       = "query"
)

// MaxMessageLen bounds a message, as OpenSSH's own agent does
// (AGENT_MAX_LEN).
const MaxMessageLen = 256 * 1024

// ReadMessage reads one message: a big-endian uint32 length, then that many
// bytes. It returns io.EOF when r ends before a message starts. An empty
// message, or one over MaxMessageLen, is an error, and nothing more of it is
// read.
func ReadMessage(r io.Reader) ([]byte, error) {
	var prefix [4]byte
	if _, err := io.ReadFull(r, prefix[:]); err != nil {
		return nil, err
	}
	n := binary.BigEndian.Uint32(prefix[:])
	if n == 0 || n > MaxMessageLen {
		return nil, fmt.Errorf("SSH agent message of %d bytes (the limit is %d)", n, MaxMessageLen)
	}
	msg := make([]byte, n)
	if _, err := io.ReadFull(r, msg); err != nil {
		if errors.Is(err, io.EOF) {
			err = io.ErrUnexpectedEOF
		}
		return nil, err
	}
	return msg, nil
}

// WriteMessage writes msg with its length prefix, in one write.
func WriteMessage(w io.Writer, msg []byte) error {
	if len(msg) == 0 || len(msg) > MaxMessageLen {
		return fmt.Errorf("SSH agent message of %d bytes (the limit is %d)", len(msg), MaxMessageLen)
	}
	buf := make([]byte, 4+len(msg))
	binary.BigEndian.PutUint32(buf, uint32(len(msg)))
	copy(buf[4:], msg)
	_, err := w.Write(buf)
	return err
}

// Type is msg's type, 0 for an empty message.
func Type(msg []byte) byte {
	if len(msg) == 0 {
		return 0
	}
	return msg[0]
}

// Failure is SSH_AGENT_FAILURE, the answer to a request that is refused or
// cannot be served.
func Failure() []byte { return []byte{MsgFailure} }

// NoIdentities is an identities answer that lists no keys: what a program
// gets when no agent is there to ask.
func NoIdentities() []byte { return []byte{MsgIdentitiesAnswer, 0, 0, 0, 0} }

// Forwardable reports whether msg may reach a client's agent: listing keys,
// signing, and the session-bind and query extensions. Anything else (adding,
// removing or locking keys, other extensions) is refused, so the devbox can
// use the keys but never change the agent that holds them.
func Forwardable(msg []byte) bool {
	switch Type(msg) {
	case MsgRequestIdentities, MsgSignRequest:
		return true
	case MsgExtension:
		name, ok := ExtensionName(msg)
		return ok && (name == ExtSessionBind || name == ExtQuery)
	}
	return false
}

// ExtensionName is the name of an extension request.
func ExtensionName(msg []byte) (string, bool) {
	if Type(msg) != MsgExtension {
		return "", false
	}
	r := wire{b: msg[1:]}
	name := r.string()
	return string(name), !r.failed
}

// Describe says what a request asks for, for people: "add a key", "lock
// the agent".
func Describe(msg []byte) string {
	switch Type(msg) {
	case MsgRequestIdentities:
		return "list keys"
	case MsgSignRequest:
		return "sign"
	case MsgAddIdentity, MsgAddIDConstrained:
		return "add a key"
	case MsgRemoveIdentity:
		return "remove a key"
	case MsgRemoveAllIdentities:
		return "remove every key"
	case MsgAddSmartcardKey, MsgAddSmartcardKeyConstrained:
		return "add a smartcard key"
	case MsgRemoveSmartcardKey:
		return "remove a smartcard key"
	case MsgLock:
		return "lock the agent"
	case MsgUnlock:
		return "unlock the agent"
	case MsgExtension:
		if name, ok := ExtensionName(msg); ok {
			return "use the extension " + Printable([]byte(name))
		}
		return "use an extension"
	case msgSSH1RequestRSAIdentities, msgSSH1RSAChallenge, msgSSH1AddRSAIdentity,
		msgSSH1RemoveRSAIdentity, msgSSH1RemoveAllRSAIdentities, msgSSH1AddRSAIDConstrained:
		return "use the obsolete SSH-1 agent protocol"
	}
	return fmt.Sprintf("send message type %d", Type(msg))
}

// Fingerprint is a key's SHA256 fingerprint as OpenSSH prints it: "SHA256:"
// and unpadded base64. A certificate is fingerprinted by its key, as
// `ssh-add -l` does, so a key's fingerprint also matches its certificates.
func Fingerprint(blob []byte) string {
	if pub, err := ssh.ParsePublicKey(blob); err == nil {
		if cert, ok := pub.(*ssh.Certificate); ok {
			blob = cert.Key.Marshal()
		}
	}
	sum := sha256.Sum256(blob)
	return "SHA256:" + base64.RawStdEncoding.EncodeToString(sum[:])
}

// ValidFingerprint reports whether s is a SHA256 fingerprint as Fingerprint
// writes it.
func ValidFingerprint(s string) bool {
	b64, ok := strings.CutPrefix(s, "SHA256:")
	if !ok {
		return false
	}
	sum, err := base64.RawStdEncoding.DecodeString(b64)
	return err == nil && len(sum) == sha256.Size
}

// Identity is one key of an identities answer.
type Identity struct {
	Blob    []byte
	Comment string
}

// ParseIdentities reads an identities answer.
func ParseIdentities(msg []byte) ([]Identity, error) {
	if Type(msg) != MsgIdentitiesAnswer {
		return nil, errors.New("not an identities answer")
	}
	r := wire{b: msg[1:]}
	n := r.uint32()
	var ids []Identity
	for i := uint32(0); i < n && !r.failed; i++ {
		blob := r.string()
		comment := r.string()
		ids = append(ids, Identity{Blob: blob, Comment: string(comment)})
	}
	if !r.done() {
		return nil, errors.New("malformed identities answer")
	}
	return ids, nil
}

// IdentitiesAnswer encodes an identities answer listing ids.
func IdentitiesAnswer(ids []Identity) []byte {
	var w bytes.Buffer
	w.WriteByte(MsgIdentitiesAnswer)
	putUint32(&w, uint32(len(ids)))
	for _, id := range ids {
		putString(&w, id.Blob)
		putString(&w, []byte(id.Comment))
	}
	return w.Bytes()
}

// SignRequest is a request to sign Data with Key.
type SignRequest struct {
	Key   []byte
	Data  []byte
	Flags uint32
}

// ParseSignRequest reads a sign request.
func ParseSignRequest(msg []byte) (*SignRequest, error) {
	if Type(msg) != MsgSignRequest {
		return nil, errors.New("not a sign request")
	}
	r := wire{b: msg[1:]}
	req := &SignRequest{Key: r.string(), Data: r.string(), Flags: r.uint32()}
	if !r.done() {
		return nil, errors.New("malformed sign request")
	}
	return req, nil
}

// SessionBind is a session-bind@openssh.com extension request: ssh binding
// its agent connection to the server it is authenticating to, by that
// server's host key, the session's id, and the host key's signature over the
// id. Forwarding is set when ssh is forwarding the agent on to that server.
type SessionBind struct {
	HostKey    []byte
	SessionID  []byte
	Signature  []byte
	Forwarding bool
}

// ParseSessionBind reads a session-bind@openssh.com extension request.
func ParseSessionBind(msg []byte) (*SessionBind, error) {
	if name, ok := ExtensionName(msg); !ok || name != ExtSessionBind {
		return nil, errors.New("not a session-bind request")
	}
	r := wire{b: msg[1:]}
	r.string() // the extension's name
	b := &SessionBind{HostKey: r.string(), SessionID: r.string(), Signature: r.string(), Forwarding: r.bool()}
	if !r.done() {
		return nil, errors.New("malformed session-bind request")
	}
	return b, nil
}

// Request is b as a session-bind@openssh.com extension request.
func (b *SessionBind) Request() []byte {
	var w bytes.Buffer
	w.WriteByte(MsgExtension)
	putString(&w, []byte(ExtSessionBind))
	putString(&w, b.HostKey)
	putString(&w, b.SessionID)
	putString(&w, b.Signature)
	if b.Forwarding {
		w.WriteByte(1)
	} else {
		w.WriteByte(0)
	}
	return w.Bytes()
}

// Purpose is what signed data is for, as far as it can be told from the data.
type Purpose struct {
	// Login is set for an SSH user authentication (RFC 4252, section 7):
	// ssh signing in to a server as User, in the session SessionID.
	Login     bool
	User      string
	SessionID []byte
	// Namespace is set for an SSHSIG signature (`ssh-keygen -Y sign`): git
	// signs commits and tags under the namespace "git".
	Namespace string
}

// sshsigMagic starts the data of every SSHSIG signature.
var sshsigMagic = []byte("SSHSIG")

// msgUserauthRequest is SSH_MSG_USERAUTH_REQUEST, which starts the data
// signed for a user authentication after the session id.
const msgUserauthRequest = 50

// ParsePurpose tells what data, the data of a sign request, is for.
func ParsePurpose(data []byte) Purpose {
	if rest, ok := bytes.CutPrefix(data, sshsigMagic); ok {
		r := wire{b: rest}
		if ns := r.string(); !r.failed {
			return Purpose{Namespace: Printable(ns)}
		}
		return Purpose{}
	}
	r := wire{b: data}
	sessionID := r.string()
	if r.byte() != msgUserauthRequest {
		return Purpose{}
	}
	user := r.string()
	r.string() // the service, "ssh-connection"
	method := string(r.string())
	if r.failed || (method != "publickey" && method != "publickey-hostbound-v00@openssh.com") {
		return Purpose{}
	}
	return Purpose{Login: true, User: Printable(user), SessionID: sessionID}
}

// Keys is an allow-list of keys by fingerprint ([ssh_agent] keys). An empty
// list allows every key.
type Keys map[string]bool

// NewKeys is the allow-list of the keys with these fingerprints.
func NewKeys(fingerprints []string) Keys {
	k := Keys{}
	for _, fp := range fingerprints {
		k[fp] = true
	}
	return k
}

// Allows reports whether the key blob may be offered and used.
func (k Keys) Allows(blob []byte) bool { return len(k) == 0 || k[Fingerprint(blob)] }

// FilterIdentities removes the keys k does not allow from an identities
// answer. With a list, an answer that cannot be read lists nothing rather
// than everything.
func (k Keys) FilterIdentities(answer []byte) []byte {
	if len(k) == 0 {
		return answer
	}
	ids, err := ParseIdentities(answer)
	if err != nil {
		return NoIdentities()
	}
	kept := ids[:0]
	for _, id := range ids {
		if k.Allows(id.Blob) {
			kept = append(kept, id)
		}
	}
	return IdentitiesAnswer(kept)
}

// KnownHostName names a host key from known_hosts files, the first host
// name given for it in a plain (not hashed) entry. It returns "" when no
// file names it, with hashed set when an entry lists the key under hashed
// names only (HashKnownHosts, the default on Debian and Ubuntu), which
// cannot be read back.
func KnownHostName(hostKey []byte, files ...string) (name string, hashed bool) {
	for _, path := range files {
		f, err := os.Open(path)
		if err != nil {
			continue
		}
		name, inFile := knownHostName(f, hostKey)
		f.Close()
		if name != "" {
			return name, false
		}
		hashed = hashed || inFile
	}
	return "", hashed
}

func knownHostName(r io.Reader, hostKey []byte) (name string, hashed bool) {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64*1024), 1<<20)
	for scanner.Scan() {
		// A marked entry is a revoked key or a certificate authority's,
		// neither of which names a host.
		marker, hosts, key, _, _, err := ssh.ParseKnownHosts(scanner.Bytes())
		if err != nil || marker != "" || !bytes.Equal(key.Marshal(), hostKey) {
			continue
		}
		for _, h := range hosts {
			if !strings.HasPrefix(h, "|") {
				return Printable([]byte(h)), false
			}
			hashed = true
		}
	}
	return "", hashed
}

// maxPrintable bounds text taken from messages into what people read.
const maxPrintable = 64

// Printable is text from a message made safe to show: invalid UTF-8 and
// control characters become '?', and it is cut at 64 runes.
func Printable(b []byte) string {
	var out strings.Builder
	n := 0
	for len(b) > 0 && n < maxPrintable {
		r, size := utf8.DecodeRune(b)
		b = b[size:]
		if r == utf8.RuneError || !unicode.IsPrint(r) {
			r = '?'
		}
		out.WriteRune(r)
		n++
	}
	if len(b) > 0 {
		out.WriteString("…")
	}
	return out.String()
}

// wire reads the SSH wire encoding (RFC 4251, section 5). Once a read runs
// past the end, failed is set and every later read returns zero values.
type wire struct {
	b      []byte
	failed bool
}

func (r *wire) take(n int) []byte {
	if r.failed || n < 0 || n > len(r.b) {
		r.failed = true
		return nil
	}
	v := r.b[:n]
	r.b = r.b[n:]
	return v
}

func (r *wire) byte() byte {
	b := r.take(1)
	if b == nil {
		return 0
	}
	return b[0]
}

func (r *wire) bool() bool { return r.byte() != 0 }

func (r *wire) uint32() uint32 {
	b := r.take(4)
	if b == nil {
		return 0
	}
	return binary.BigEndian.Uint32(b)
}

func (r *wire) string() []byte {
	n := r.uint32()
	if r.failed || uint64(n) > uint64(len(r.b)) {
		r.failed = true
		return nil
	}
	return r.take(int(n))
}

// done reports whether everything was read, and nothing more is left.
func (r *wire) done() bool { return !r.failed && len(r.b) == 0 }

func putUint32(w *bytes.Buffer, v uint32) {
	var b [4]byte
	binary.BigEndian.PutUint32(b[:], v)
	w.Write(b[:])
}

func putString(w *bytes.Buffer, s []byte) {
	putUint32(w, uint32(len(s)))
	w.Write(s)
}
