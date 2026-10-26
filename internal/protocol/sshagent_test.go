package protocol

import (
	"slices"
	"testing"
)

func TestSSHAgentRoundTrips(t *testing.T) {
	roundTripRequest(t, &Request{ForwardSSHAgent: &ForwardSSHAgent{SessionID: "demo", ExpectUID: "00ff", Attachment: "attach-3", Client: "laptop (SHA256:abc)"}})
	roundTripRequest(t, &Request{ForwardSSHAgent: &ForwardSSHAgent{SessionID: "demo", Features: []string{"future"}}})
	roundTripRequest(t, &Request{SSHAgentReply: &SSHAgentMessage{Conn: 7, Data: []byte{12, 0, 0, 0, 0}}})
	roundTripRequest(t, &Request{SSHAgentClose: &SSHAgentConn{Conn: 7}})
	roundTripResponse(t, &Response{SSHAgentForwarding: &SSHAgentForwarding{}})
	roundTripResponse(t, &Response{SSHAgentForwarding: &SSHAgentForwarding{Features: []string{"future"}}})
	roundTripResponse(t, &Response{SSHAgentRequest: &SSHAgentMessage{Conn: 1, Data: []byte{11}}})
	roundTripResponse(t, &Response{SSHAgentClose: &SSHAgentConn{Conn: 1}})
}

// TestSSHAgentGoldenFrames pins the SSH agent forwarding messages: kinds
// 80-82 from the client, 180-182 from the worker.
func TestSSHAgentGoldenFrames(t *testing.T) {
	assertRequestGolden(t, &Request{ForwardSSHAgent: &ForwardSSHAgent{SessionID: "s", ExpectUID: "u", Attachment: "a", Client: "c", Features: []string{"f"}}}, []byte{
		0x50, 0x44, 0x47, 0x41, 0x01, 0x00, 0x50, 0x00, 0x00, 0x00, 0x00, 0x00, // kind 80
		0x1d, 0x00, 0x00, 0x00,
		0x01, 0x00, 0x00, 0x00, 's', // session
		0x01, 0x00, 0x00, 0x00, 'u', // expected UID
		0x01, 0x00, 0x00, 0x00, 'a', // attachment
		0x01, 0x00, 0x00, 0x00, 'c', // client
		0x01, 0x00, 0x00, 0x00, 0x01, 0x00, 0x00, 0x00, 'f', // features
	})
	assertRequestGolden(t, &Request{SSHAgentReply: &SSHAgentMessage{Conn: 0x01020304, Data: []byte{5}}}, []byte{
		0x50, 0x44, 0x47, 0x41, 0x01, 0x00, 0x51, 0x00, 0x00, 0x00, 0x00, 0x00, // kind 81
		0x09, 0x00, 0x00, 0x00,
		0x04, 0x03, 0x02, 0x01, // conn
		0x01, 0x00, 0x00, 0x00, 0x05, // SSH_AGENT_FAILURE
	})
	assertRequestGolden(t, &Request{SSHAgentClose: &SSHAgentConn{Conn: 2}}, []byte{
		0x50, 0x44, 0x47, 0x41, 0x01, 0x00, 0x52, 0x00, 0x00, 0x00, 0x00, 0x00, // kind 82
		0x04, 0x00, 0x00, 0x00,
		0x02, 0x00, 0x00, 0x00,
	})
	assertResponseGolden(t, &Response{SSHAgentForwarding: &SSHAgentForwarding{}}, []byte{
		0x50, 0x44, 0x47, 0x41, 0x01, 0x00, 0xb4, 0x00, 0x00, 0x00, 0x00, 0x00, // kind 180
		0x04, 0x00, 0x00, 0x00,
		0x00, 0x00, 0x00, 0x00, // no features
	})
	assertResponseGolden(t, &Response{SSHAgentRequest: &SSHAgentMessage{Conn: 3, Data: []byte{11}}}, []byte{
		0x50, 0x44, 0x47, 0x41, 0x01, 0x00, 0xb5, 0x00, 0x00, 0x00, 0x00, 0x00, // kind 181
		0x09, 0x00, 0x00, 0x00,
		0x03, 0x00, 0x00, 0x00,
		0x01, 0x00, 0x00, 0x00, 0x0b, // SSH_AGENTC_REQUEST_IDENTITIES
	})
	assertResponseGolden(t, &Response{SSHAgentClose: &SSHAgentConn{Conn: 3}}, []byte{
		0x50, 0x44, 0x47, 0x41, 0x01, 0x00, 0xb6, 0x00, 0x00, 0x00, 0x00, 0x00, // kind 182
		0x04, 0x00, 0x00, 0x00,
		0x03, 0x00, 0x00, 0x00,
	})
}

// A forwarder is a stream of its own, offered under its own capability and
// never as an attach feature: it is not an attach stream.
func TestSSHAgentStreamsHaveTheirOwnRole(t *testing.T) {
	if got := (&Request{ForwardSSHAgent: &ForwardSSHAgent{}}).Role(); got != RoleSSHAgent || got.String() != "ssh-agent" {
		t.Fatalf("role = %v", got)
	}
	for _, req := range []*Request{{SSHAgentReply: &SSHAgentMessage{}}, {SSHAgentClose: &SSHAgentConn{}}} {
		if got := req.Role(); got != RoleRequest {
			t.Fatalf("%#v: role %v", req, got)
		}
	}
	if !slices.Contains(Capabilities(), CapSSHAgent) || slices.Contains(AttachCapabilities(), CapSSHAgent) {
		t.Fatal("ssh-agent is a capability of its own, not an attach feature")
	}
}
