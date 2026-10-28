package protocol

import (
	"bytes"
	"reflect"
	"testing"
	"time"

	"agtd.sh/internal/session"
)

// The decoders read bytes straight off the network. These fuzz tests check
// that no input makes them panic or allocate without bound, and that
// whatever they accept encodes back to a frame that decodes to the same
// value. Run them longer with, for example:
//
//	go test ./internal/protocol -run '^$' -fuzz FuzzReadRequest -fuzztime 5m

func seedFrames(f *testing.F) {
	now := time.Unix(1_700_000_000, 5).UTC()
	reqs := []*Request{
		{Hello: NewHello("seed")},
		{CreateSession: &CreateSession{Cwd: "/w", Name: strp("fix"), Agent: "codex", Workspace: strp("ws")}},
		{AttachSession: &AttachSession{SessionID: "ab", Kind: session.AttachmentAttach, Geometry: Geometry{80, 24, 0, 0}}},
		{AttachSession: &AttachSession{SessionID: "ab", Kind: session.AttachmentAttach, Features: AttachCapabilities(), ExpectUID: "u", Replaces: "attach-1", Colors: &TerminalColors{Foreground: RGB{1, 2, 3}, Background: RGB{4, 5, 6}}}},
		{AttachFrameAck: &FrameAck{Seq: 3, Field: true}},
		{AttachCompose: &Bytes{Data: []byte("run the tests")}},
		{SendInput: &SendInput{SessionID: "ab", Data: []byte("hi\r")}},
		{SendInput: &SendInput{SessionID: "ab", Data: []byte("run the tests"), Compose: true}},
		{ListSessions: Empty},
		{GetGitState: &SessionRef{"ab"}},
		{GetArtifact: &GetArtifact{SessionID: "ab", Name: "diff"}},
		{GetSessionStats: &GetSessionStats{SessionID: "ab", Snapshot: true}},
		{SubscribeEvents: &SubscribeEvents{AfterID: u64p(7), SessionID: strp("ab"), Tail: 3}},
		{ListEvents: &ListEvents{Limit: 5}},
		{HandoffSession: &HandoffSession{SessionID: "ab", Executable: "/bin/agtd"}},
		{AttachTakeSize: &Geometry{Cols: 120, Rows: 40}},
		{AttachLeave: Empty},
		{AttachField: Empty},
		{ForwardSSHAgent: &ForwardSSHAgent{SessionID: "ab", ExpectUID: "u", Attachment: "attach-1", Client: "local", Features: []string{"f"}}},
		{SSHAgentReply: &SSHAgentMessage{Conn: 2, Data: []byte{12, 0, 0, 0, 0}}},
		{SSHAgentClose: &SSHAgentConn{Conn: 2}},
	}
	resps := []*Response{
		{Welcome: &Welcome{Version: 1, Capabilities: []string{CapControlStream}, Host: HostInfo{Agents: []string{"a"}}}},
		{Sessions: &[]session.Record{{SessionID: "ab", Agent: "sh", Mode: session.ModeExecute, Status: session.StatusRunning, Attention: session.AttentionInfo, CreatedAt: now, UpdatedAt: now}}},
		{Attached: &Attached{AttachID: "attach-1", Snapshot: []byte("\x1b[H")}},
		{Attached: &Attached{AttachID: "attach-1", Features: []string{CapSessionUID, CapAttachResync}, SessionUID: "u"}},
		{AttachResync: &Bytes{Data: []byte("\x1b[2J")}},
		{AttachFrame: &Frame{Seq: 2, Full: true, Cols: 80, Lines: 24, Scrolled: 1, CursorRow: 3, CursorCol: 4, CursorShape: 5, Prefix: []byte("\x1b[?2004h\x1b[H"), Keyboard: []byte("\x1b[=1;1u"), Colors: []byte("\x1b]11;#000000\x1b\\"), Effects: []byte("\a"), Field: &Field{Cols: 80, Lines: 24}, Rows: []FrameRow{{Index: 0, VT: []byte("$ ")}, {Index: 1}}, History: [][]byte{[]byte("gone"), nil}}},
		{AttachFrame: &Frame{Seq: 4, Acked: 2, HasAcked: true}},
		{AttachFrame: &Frame{Seq: 4, Acked: 2, HasAcked: true, HasRecord: true, Record: &FrameRecord{Pos: 3, Mark: 9, End: 20, Rows: [][]byte{[]byte("row")}}}},
		{AttachSizeOwner: &SizeOwnership{Owner: true, OwnerID: "attach-1", Geometry: Geometry{80, 24, 0, 0}}},
		{Field: &Field{Found: true, Cols: 120, Lines: 40, Alternate: true, Row: 36, Col: 2, TextCol: 2, Width: 117, GrowsUp: true,
			Rows: []string{"hello"}, Style: []byte("\x1b[0m"), Chrome: []ScreenRow{{Index: 35, VT: []byte("\u2500")}}}},
		{Field: &Field{Cols: 80, Lines: 24}},
		{Attachments: &[]session.AttachmentRecord{{AttachID: "a", SessionID: "b", Kind: session.AttachmentTui, ConnectedAt: now}}},
		ErrorResponsef("no"),
		{GitState: sampleGitState()},
		{Artifacts: &[]session.Artifact{{Name: "diff", Kind: "diff", Size: u64p(9)}}},
		{ArtifactChunk: &Bytes{Data: []byte("@@ -1 +1 @@")}},
		{SessionStats: &SessionStats{SessionID: "ab", Worker: ProcessStats{PID: 1, RSSBytes: 2}, Snapshot: &SnapshotStats{Bytes: 3}}},
		{DaemonStats: &DaemonStats{Daemon: ProcessStats{PID: 1}, OpenStreams: 2}},
		{Event: &session.Event{ID: 9, SessionID: "ab", At: now, Kind: session.EventBell, Attention: session.AttentionAction, Summary: "bell"}},
		{Events: &[]session.Event{{ID: 1, SessionID: "ab", At: now, Kind: "future-kind", Attention: session.AttentionInfo}}},
		{HandedOff: &HandedOff{SessionID: "ab", WorkerPID: 7, Executable: "/bin/agtd", Handoffs: 1}},
		{SessionRestarting: &SessionRestarting{SessionID: "ab", Reason: "upgrade"}},
		{SSHAgentForwarding: &SSHAgentForwarding{Features: []string{"f"}}},
		{SSHAgentRequest: &SSHAgentMessage{Conn: 1, Data: []byte{11}}},
		{SSHAgentClose: &SSHAgentConn{Conn: 1}},
	}
	for _, r := range reqs {
		var buf bytes.Buffer
		WriteRequest(&buf, r)
		f.Add(buf.Bytes())
		buf.Reset()
		WriteTaggedRequest(&buf, Features{}, 3, r)
		f.Add(buf.Bytes())
	}
	for _, r := range resps {
		var buf bytes.Buffer
		WriteResponse(&buf, r)
		f.Add(buf.Bytes())
		buf.Reset()
		WriteTaggedResponse(&buf, Features{}, 3, r)
		f.Add(buf.Bytes())
		buf.Reset()
		WriteTaggedResponse(&buf, allFeatures(), 3, r)
		f.Add(buf.Bytes())
	}
	var buf bytes.Buffer
	WriteManagementRequest(&buf, &ManagementRequest{Shutdown: &ManagementShutdown{Force: true}})
	f.Add(buf.Bytes())
}

func FuzzReadRequest(f *testing.F) {
	seedFrames(f)
	f.Fuzz(func(t *testing.T, data []byte) {
		req, id, tagged, err := ReadTaggedRequest(bytes.NewReader(data), Features{})
		if err != nil || req == nil {
			return
		}
		var buf bytes.Buffer
		if tagged {
			err = WriteTaggedRequest(&buf, Features{}, id, req)
		} else {
			err = WriteRequest(&buf, req)
		}
		if err != nil {
			t.Fatalf("decoded request does not encode: %v (%#v)", err, req)
		}
		again, id2, tagged2, err := ReadTaggedRequest(&buf, Features{})
		if err != nil || id2 != id || tagged2 != tagged || !reflect.DeepEqual(again, req) {
			t.Fatalf("request round trip changed it:\n%#v\n%#v (%v)", req, again, err)
		}
	})
}

func FuzzReadResponse(f *testing.F) {
	seedFrames(f)
	f.Fuzz(func(t *testing.T, data []byte) {
		// Responses are decoded both in the base encoding and with every
		// optional field.
		for _, features := range []Features{{}, allFeatures()} {
			resp, id, tagged, err := ReadTaggedResponse(bytes.NewReader(data), features)
			if err != nil || resp == nil {
				continue
			}
			var buf bytes.Buffer
			if tagged {
				err = WriteTaggedResponse(&buf, features, id, resp)
			} else {
				err = WriteResponseWith(&buf, features, resp)
			}
			if err != nil {
				t.Fatalf("decoded response does not encode: %v (%#v)", err, resp)
			}
			again, id2, tagged2, err := ReadTaggedResponse(&buf, features)
			if err != nil || id2 != id || tagged2 != tagged || !reflect.DeepEqual(again, resp) {
				t.Fatalf("response round trip changed it:\n%#v\n%#v (%v)", resp, again, err)
			}
		}
	})
}

func allFeatures() Features { return NegotiateFeatures(Capabilities(), Capabilities()) }

// FuzzReadIncoming covers the daemon's first read, which also accepts the
// JSON management protocol.
func FuzzReadIncoming(f *testing.F) {
	seedFrames(f)
	f.Fuzz(func(t *testing.T, data []byte) {
		req, mgmt, err := ReadIncoming(bytes.NewReader(data))
		if err != nil {
			return
		}
		if req != nil && mgmt != nil {
			t.Fatal("ReadIncoming returned both a request and a management request")
		}
		if mgmt != nil {
			var buf bytes.Buffer
			if err := WriteManagementRequest(&buf, mgmt); err != nil {
				t.Fatalf("decoded management request does not encode: %v", err)
			}
		}
	})
}

// Pairing frames come from peers that are not authenticated yet.
func FuzzReadPairMessage(f *testing.F) {
	for _, m := range []*PairMessage{
		{Hello: &PairHello{Name: "laptop", Message: []byte("a")}},
		{Proof: &PairProof{MAC: []byte("c")}},
		{Error: &ErrorResponse{Message: "no"}},
	} {
		var buf bytes.Buffer
		WritePairMessage(&buf, m)
		f.Add(buf.Bytes())
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		m, err := ReadPairMessage(bytes.NewReader(data))
		if err == nil && m.Hello == nil && m.Challenge == nil && m.Proof == nil && m.Accepted == nil && m.Error == nil {
			t.Fatal("decoded an empty pairing message")
		}
	})
}
