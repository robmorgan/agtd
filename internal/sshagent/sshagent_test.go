package sshagent

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/binary"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"
)

func newKey(t testing.TB) ssh.PublicKey {
	t.Helper()
	pub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	key, err := ssh.NewPublicKey(pub)
	if err != nil {
		t.Fatal(err)
	}
	return key
}

// message builds a message from its type and SSH wire fields: []byte and
// string as strings, uint32 and bool as themselves.
func message(typ byte, fields ...any) []byte {
	var w bytes.Buffer
	w.WriteByte(typ)
	for _, f := range fields {
		switch v := f.(type) {
		case []byte:
			putString(&w, v)
		case string:
			putString(&w, []byte(v))
		case uint32:
			putUint32(&w, v)
		case bool:
			if v {
				w.WriteByte(1)
			} else {
				w.WriteByte(0)
			}
		case byte:
			w.WriteByte(v)
		}
	}
	return w.Bytes()
}

func TestMessagesAreFramedAndBounded(t *testing.T) {
	var buf bytes.Buffer
	if err := WriteMessage(&buf, []byte{MsgRequestIdentities}); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(buf.Bytes(), []byte{0, 0, 0, 1, 11}) {
		t.Fatalf("framed as % x", buf.Bytes())
	}
	msg, err := ReadMessage(&buf)
	if err != nil || !bytes.Equal(msg, []byte{11}) {
		t.Fatalf("read back %v, %v", msg, err)
	}
	if _, err := ReadMessage(&buf); err != io.EOF {
		t.Fatalf("clean end = %v", err)
	}

	big := make([]byte, 4+MaxMessageLen+1)
	binary.BigEndian.PutUint32(big, MaxMessageLen+1)
	if _, err := ReadMessage(bytes.NewReader(big)); err == nil {
		t.Fatal("an oversized message was read")
	}
	if _, err := ReadMessage(bytes.NewReader([]byte{0, 0, 0, 0})); err == nil {
		t.Fatal("an empty message was read")
	}
	if _, err := ReadMessage(bytes.NewReader([]byte{0, 0, 0, 3, 11})); err != io.ErrUnexpectedEOF {
		t.Fatalf("a truncated message = %v", err)
	}
	if err := WriteMessage(io.Discard, make([]byte, MaxMessageLen+1)); err == nil {
		t.Fatal("an oversized message was written")
	}
	max := make([]byte, MaxMessageLen)
	buf.Reset()
	if err := WriteMessage(&buf, max); err != nil {
		t.Fatal(err)
	}
	if msg, err := ReadMessage(&buf); err != nil || len(msg) != MaxMessageLen {
		t.Fatalf("a message at the limit: %d bytes, %v", len(msg), err)
	}
}

// Only listing, signing, and the session-bind and query extensions reach
// a client's agent.
func TestForwardable(t *testing.T) {
	for _, tc := range []struct {
		msg  []byte
		want bool
		says string
	}{
		{[]byte{MsgRequestIdentities}, true, "list keys"},
		{message(MsgSignRequest, []byte("k"), []byte("d"), uint32(0)), true, "sign"},
		{message(MsgExtension, ExtSessionBind, []byte("h"), []byte("s"), []byte("sig"), false), true, "use the extension session-bind@openssh.com"},
		{message(MsgExtension, ExtQuery), true, "use the extension query"},
		{message(MsgExtension, "frobnicate@example.com"), false, "use the extension frobnicate@example.com"},
		{[]byte{MsgExtension, 0, 0}, false, "use an extension"},
		{message(MsgAddIdentity, "ssh-ed25519"), false, "add a key"},
		{message(MsgAddIDConstrained, "ssh-ed25519"), false, "add a key"},
		{message(MsgRemoveIdentity, []byte("k")), false, "remove a key"},
		{[]byte{MsgRemoveAllIdentities}, false, "remove every key"},
		{message(MsgLock, "pw"), false, "lock the agent"},
		{message(MsgUnlock, "pw"), false, "unlock the agent"},
		{message(MsgAddSmartcardKey, "id", "pin"), false, "add a smartcard key"},
		{message(MsgRemoveSmartcardKey, "id", "pin"), false, "remove a smartcard key"},
		{[]byte{1}, false, "send message type 1"},
		{nil, false, "send message type 0"},
	} {
		if got := Forwardable(tc.msg); got != tc.want {
			t.Errorf("Forwardable(% x) = %v", tc.msg, got)
		}
		if got := Describe(tc.msg); got != tc.says {
			t.Errorf("Describe(% x) = %q, want %q", tc.msg, got, tc.says)
		}
	}
}

func TestFingerprints(t *testing.T) {
	key := newKey(t)
	fp := Fingerprint(key.Marshal())
	if fp != ssh.FingerprintSHA256(key) || !ValidFingerprint(fp) {
		t.Fatalf("fingerprint %s, x/crypto says %s", fp, ssh.FingerprintSHA256(key))
	}
	// A certificate is known by its key's fingerprint, as ssh-add -l
	// shows it.
	_, caPriv, _ := ed25519.GenerateKey(rand.Reader)
	ca, err := ssh.NewSignerFromKey(caPriv)
	if err != nil {
		t.Fatal(err)
	}
	cert := &ssh.Certificate{Key: key, CertType: ssh.UserCert, ValidPrincipals: []string{"git"}, ValidBefore: ssh.CertTimeInfinity}
	if err := cert.SignCert(rand.Reader, ca); err != nil {
		t.Fatal(err)
	}
	if got := Fingerprint(cert.Marshal()); got != fp {
		t.Fatalf("certificate fingerprint %s, key's %s", got, fp)
	}
	// Bytes that are no key still get a fingerprint, of the bytes.
	if got := Fingerprint([]byte("junk")); !ValidFingerprint(got) || got == fp {
		t.Fatalf("fingerprint of junk = %s", got)
	}
	for _, bad := range []string{"", "SHA256:", "MD5:aa", "SHA256:not base64!", fp + "AA", strings.TrimPrefix(fp, "SHA256:")} {
		if ValidFingerprint(bad) {
			t.Errorf("%q is valid", bad)
		}
	}
}

func TestIdentitiesAndTheKeyList(t *testing.T) {
	a, b := newKey(t), newKey(t)
	answer := IdentitiesAnswer([]Identity{{Blob: a.Marshal(), Comment: "a@laptop"}, {Blob: b.Marshal(), Comment: "b@laptop"}})
	ids, err := ParseIdentities(answer)
	if err != nil || len(ids) != 2 || ids[1].Comment != "b@laptop" || !bytes.Equal(ids[0].Blob, a.Marshal()) {
		t.Fatalf("parsed %+v, %v", ids, err)
	}
	if ids, err := ParseIdentities(NoIdentities()); err != nil || len(ids) != 0 {
		t.Fatalf("no identities: %+v, %v", ids, err)
	}
	for _, bad := range [][]byte{nil, {MsgIdentitiesAnswer}, {MsgIdentitiesAnswer, 0, 0, 0, 1}, append(NoIdentities(), 0), {MsgFailure}} {
		if _, err := ParseIdentities(bad); err == nil {
			t.Errorf("parsed % x", bad)
		}
	}

	only := NewKeys([]string{Fingerprint(b.Marshal())})
	if only.Allows(a.Marshal()) || !only.Allows(b.Marshal()) {
		t.Fatal("the list allows the wrong key")
	}
	ids, err = ParseIdentities(only.FilterIdentities(answer))
	if err != nil || len(ids) != 1 || ids[0].Comment != "b@laptop" {
		t.Fatalf("filtered %+v, %v", ids, err)
	}
	if got := only.FilterIdentities([]byte{MsgIdentitiesAnswer, 9}); !bytes.Equal(got, NoIdentities()) {
		t.Fatalf("an unreadable answer filtered to % x", got)
	}
	all := NewKeys(nil)
	if !all.Allows(a.Marshal()) || !bytes.Equal(all.FilterIdentities(answer), answer) {
		t.Fatal("an empty list does not allow everything")
	}
}

func TestSignRequestsAndTheirPurpose(t *testing.T) {
	key := newKey(t)
	login := message(0, []byte("session-id"), byte(msgUserauthRequest), "git", "ssh-connection", "publickey", true, "ssh-ed25519", key.Marshal())[1:]
	req, err := ParseSignRequest(message(MsgSignRequest, key.Marshal(), login, uint32(4)))
	if err != nil || !bytes.Equal(req.Key, key.Marshal()) || req.Flags != 4 {
		t.Fatalf("parsed %+v, %v", req, err)
	}
	if p := ParsePurpose(req.Data); !p.Login || p.User != "git" || string(p.SessionID) != "session-id" {
		t.Fatalf("login purpose = %+v", p)
	}
	hostbound := message(0, []byte("sid"), byte(msgUserauthRequest), "rob", "ssh-connection", "publickey-hostbound-v00@openssh.com", true)[1:]
	if p := ParsePurpose(hostbound); !p.Login || p.User != "rob" {
		t.Fatalf("host-bound login purpose = %+v", p)
	}
	password := message(0, []byte("sid"), byte(msgUserauthRequest), "rob", "ssh-connection", "password")[1:]
	if p := ParsePurpose(password); p.Login {
		t.Fatalf("a password request is a login: %+v", p)
	}
	sshsig := append([]byte("SSHSIG"), message(0, "git", "", "sha512", []byte("hash"))[1:]...)
	if p := ParsePurpose(sshsig); p.Namespace != "git" || p.Login {
		t.Fatalf("SSHSIG purpose = %+v", p)
	}
	if p := ParsePurpose([]byte("arbitrary bytes")); p.Login || p.Namespace != "" {
		t.Fatalf("arbitrary data purpose = %+v", p)
	}
	for _, bad := range [][]byte{{MsgSignRequest}, message(MsgSignRequest, key.Marshal()), append(message(MsgSignRequest, key.Marshal(), []byte("d"), uint32(0)), 0), {MsgFailure}} {
		if _, err := ParseSignRequest(bad); err == nil {
			t.Errorf("parsed % x", bad)
		}
	}
}

func TestSessionBind(t *testing.T) {
	host := newKey(t)
	b, err := ParseSessionBind(message(MsgExtension, ExtSessionBind, host.Marshal(), []byte("sid"), []byte("sig"), true))
	if err != nil || !bytes.Equal(b.HostKey, host.Marshal()) || string(b.SessionID) != "sid" || string(b.Signature) != "sig" || !b.Forwarding {
		t.Fatalf("parsed %+v, %v", b, err)
	}
	for _, bad := range [][]byte{
		message(MsgExtension, ExtQuery),
		message(MsgExtension, ExtSessionBind, host.Marshal()),
		append(message(MsgExtension, ExtSessionBind, host.Marshal(), []byte("sid"), []byte("sig"), false), 0),
	} {
		if _, err := ParseSessionBind(bad); err == nil {
			t.Errorf("parsed % x", bad)
		}
	}
}

func TestKnownHostName(t *testing.T) {
	dir := t.TempDir()
	github, devbox, revoked, other := newKey(t), newKey(t), newKey(t), newKey(t)
	hashed := filepath.Join(dir, "known_hosts")
	plain := filepath.Join(dir, "ssh_known_hosts")
	os.WriteFile(hashed, []byte(
		"# a comment\n"+
			knownhosts.Line([]string{knownhosts.HashHostname("devbox")}, devbox)+"\n"+
			"@revoked "+knownhosts.Line([]string{"bad.example"}, revoked)+"\n"+
			"garbage line\n"), 0o600)
	os.WriteFile(plain, []byte(
		knownhosts.Line([]string{knownhosts.HashHostname("github.com"), "github.com", "140.82.112.3"}, github)+"\n"+
			knownhosts.Line([]string{"[devbox.lan]:2222"}, devbox)+"\n"), 0o600)

	for _, tc := range []struct {
		key  ssh.PublicKey
		want string
	}{
		{github, "github.com"},
		// Hashed in the first file, named in the second.
		{devbox, "[devbox.lan]:2222"},
		{revoked, ""},
		{other, ""},
	} {
		if got := KnownHostName(tc.key.Marshal(), filepath.Join(dir, "missing"), hashed, plain); got != tc.want {
			t.Errorf("name of %s = %q, want %q", ssh.FingerprintSHA256(tc.key), got, tc.want)
		}
	}
}

func TestPrintable(t *testing.T) {
	for in, want := range map[string]string{
		"git":                   "git",
		"evil\x1b[2Jname":       "evil?[2Jname",
		"tab\there":             "tab?here",
		"\xff\xfe":              "??",
		"ünïcode":               "ünïcode",
		strings.Repeat("a", 70): strings.Repeat("a", 64) + "…",
		strings.Repeat("é", 64): strings.Repeat("é", 64),
	} {
		if got := Printable([]byte(in)); got != want {
			t.Errorf("Printable(%q) = %q, want %q", in, got, want)
		}
	}
}

// The parsers read bytes from programs on the devbox and from clients:
// nothing they are given may make them panic, and what parses re-encodes
// to the same bytes.
func FuzzParse(f *testing.F) {
	key := newKey(f)
	for _, seed := range [][]byte{
		IdentitiesAnswer([]Identity{{Blob: key.Marshal(), Comment: "c"}}),
		message(MsgSignRequest, key.Marshal(), []byte("data"), uint32(2)),
		message(MsgExtension, ExtSessionBind, key.Marshal(), []byte("sid"), []byte("sig"), false),
		append([]byte("SSHSIG"), message(0, "git")[1:]...),
		{MsgRequestIdentities},
	} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, msg []byte) {
		Forwardable(msg)
		Describe(msg)
		Fingerprint(msg)
		ParsePurpose(msg)
		ParseSessionBind(msg)
		if ids, err := ParseIdentities(msg); err == nil && !bytes.Equal(IdentitiesAnswer(ids), msg) {
			t.Fatalf("identities answer % x re-encodes as % x", msg, IdentitiesAnswer(ids))
		}
		if _, err := ParseSignRequest(msg); err == nil && Type(msg) != MsgSignRequest {
			t.Fatal("parsed a sign request of another type")
		}
		NewKeys([]string{Fingerprint(key.Marshal())}).FilterIdentities(msg)
		var buf bytes.Buffer
		if WriteMessage(&buf, msg) == nil {
			if got, err := ReadMessage(&buf); err != nil || !bytes.Equal(got, msg) {
				t.Fatalf("framing round trip: % x, %v", got, err)
			}
		}
	})
}
