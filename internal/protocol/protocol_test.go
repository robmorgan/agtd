package protocol

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"reflect"
	"strings"
	"testing"
	"time"

	"agtd.sh/internal/session"
)

func strp(s string) *string { return &s }
func i32p(v int32) *int32   { return &v }
func u32p(v uint32) *uint32 { return &v }

func roundTripRequest(t *testing.T, req *Request) {
	t.Helper()
	var buf bytes.Buffer
	if err := WriteRequest(&buf, req); err != nil {
		t.Fatal(err)
	}
	got, err := ReadRequest(&buf)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, req) {
		t.Fatalf("round trip mismatch\n got: %#v\nwant: %#v", got, req)
	}
	if buf.Len() != 0 {
		t.Fatalf("trailing bytes after frame: %d", buf.Len())
	}
}

func roundTripResponse(t *testing.T, resp *Response) {
	t.Helper()
	var buf bytes.Buffer
	if err := WriteResponse(&buf, resp); err != nil {
		t.Fatal(err)
	}
	got, err := ReadResponse(&buf)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, resp) {
		t.Fatalf("round trip mismatch\n got: %#v\nwant: %#v", got, resp)
	}
}

func TestRequestRoundTrips(t *testing.T) {
	roundTripRequest(t, &Request{SendInput: &SendInput{SessionID: "demo", Data: []byte{0, 1, 2, 255}, SourceSessionID: strp("origin")}})
	roundTripRequest(t, &Request{AttachResize: &Geometry{Cols: 120, Rows: 48, PixelWidth: 960, PixelHeight: 768}})
	roundTripRequest(t, &Request{AttachSnapshot: Empty})
	roundTripRequest(t, &Request{DetachSession: &DetachSession{SessionID: "demo", All: true}})
	roundTripRequest(t, &Request{AttachSession: &AttachSession{SessionID: "demo", Kind: session.AttachmentTui, Geometry: Geometry{120, 48, 960, 768}}})
	roundTripRequest(t, &Request{DetachAttachment: &DetachAttachment{SessionID: "demo", AttachID: "attach-1"}})
	roundTripRequest(t, &Request{ListAttachments: &SessionRef{"demo"}})
	roundTripRequest(t, &Request{GetHistory: &GetHistory{SessionID: "demo", VT: true}})
	roundTripRequest(t, &Request{KillSession: &KillSession{SessionID: "demo", Remove: true}})
	roundTripRequest(t, &Request{CreateSession: &CreateSession{Cwd: "/tmp/x", Name: strp("fix"), Agent: "codex", Model: strp("m")}})
	roundTripRequest(t, &Request{CreateSession: &CreateSession{Cwd: "sub", Agent: "claude", Workspace: strp("isara")}})
	roundTripRequest(t, &Request{ListWorkspaces: Empty})
	roundTripRequest(t, &Request{AddWorkspace: &AddWorkspace{Name: "mono", Path: "~/src/mono"}})
	roundTripRequest(t, &Request{RemoveWorkspace: &WorkspaceRef{Name: "mono"}})
	roundTripRequest(t, &Request{GetDaemonInfo: Empty})
	roundTripRequest(t, &Request{AttachInput: &Bytes{Data: []byte("hi")}})
}

func TestResponseRoundTrips(t *testing.T) {
	now := time.Unix(1_700_000_000, 123_456_789).UTC()
	roundTripResponse(t, &Response{Attached: &Attached{AttachID: "attach-1", Snapshot: []byte{0, 1, 2, 3, 4}}})
	roundTripResponse(t, &Response{AttachSnapshot: &Bytes{Data: []byte{4, 3, 2, 1}}})
	roundTripResponse(t, &Response{History: &History{Data: "\x1b[31mhello\x1b[0m"}})
	roundTripResponse(t, &Response{Ok: Empty})
	roundTripResponse(t, &Response{EndOfStream: Empty})
	roundTripResponse(t, &Response{InputAccepted: Empty})
	roundTripResponse(t, &Response{Error: &ErrorResponse{Message: "boom"}})
	roundTripResponse(t, &Response{SessionEnded: &SessionEnded{
		SessionID: "demo", Status: session.StatusExited, ExitCode: i32p(0), Error: nil,
	}})
	roundTripResponse(t, &Response{CreateSession: &session.CreateResult{
		SessionID: "demo", Cwd: "/tmp/demo", Status: session.StatusRunning, Mode: session.ModeExecute,
	}})
	roundTripResponse(t, &Response{Attachments: &[]session.AttachmentRecord{{
		AttachID: "attach-1", SessionID: "demo", Kind: session.AttachmentAttach, ConnectedAt: now,
	}}})
	roundTripResponse(t, &Response{Sessions: &[]session.Record{{
		SessionID: "demo", Agent: "codex", Model: strp("gpt-5.4"), Mode: session.ModeExecute,
		Cwd: "/tmp/demo", Status: session.StatusRunning,
		WorkerPID: u32p(123), AgentPID: u32p(456), Attention: session.AttentionInfo,
		AttentionSummary: strp("fix"), CreatedAt: now, UpdatedAt: now,
	}}})
}

// TestAttachSessionGoldenFrame pins the exact bytes of an AttachSession
// request, spelling out the framing field by field.
func TestAttachSessionGoldenFrame(t *testing.T) {
	var buf bytes.Buffer
	err := WriteRequest(&buf, &Request{AttachSession: &AttachSession{
		SessionID: "ab", Kind: session.AttachmentTui, Geometry: Geometry{Cols: 0x0102, Rows: 0x0304, PixelWidth: 0x0506, PixelHeight: 0x0708},
	}})
	if err != nil {
		t.Fatal(err)
	}
	want := []byte{
		0x50, 0x44, 0x47, 0x41, // magic "AGDP" little-endian
		1, 0, // protocol version
		5, 0, // AttachSessionRequest
		0, 0, 0, 0, // flags, reserved
		15, 0, 0, 0, // payload length
		2, 0, 0, 0, 'a', 'b', // session id
		2,                      // AttachmentKind::Tui
		0x02, 0x01, 0x04, 0x03, // cols, rows
		0x06, 0x05, 0x08, 0x07, // pixel width, pixel height
	}
	if !bytes.Equal(buf.Bytes(), want) {
		t.Fatalf("golden mismatch\n got % x\nwant % x", buf.Bytes(), want)
	}
}

// Unknown kinds are refused rather than decoded into something else, and a
// frame at another protocol version is refused before its kind is looked at.
func TestUnknownKindsAndVersionsAreRejected(t *testing.T) {
	e := &encoder{}
	e.str("demo")
	payload := e.buf

	for _, k := range []uint16{0, 99, 100, 999, 9999} {
		var buf bytes.Buffer
		if err := writeFrame(&buf, ProtocolVersion, k, payload); err != nil {
			t.Fatal(err)
		}
		if _, err := ReadRequest(&buf); err == nil {
			t.Fatalf("request kind %d should be rejected", k)
		}
		buf.Reset()
		if err := writeFrame(&buf, ProtocolVersion, k, payload); err != nil {
			t.Fatal(err)
		}
		if _, err := ReadResponse(&buf); err == nil {
			t.Fatalf("response kind %d should be rejected", k)
		}
	}

	var buf bytes.Buffer
	if err := writeFrame(&buf, ProtocolVersion+1, uint16(kAttachSessionRequest), payload); err != nil {
		t.Fatal(err)
	}
	_, err := ReadRequest(&buf)
	var ve *VersionError
	if !errors.As(err, &ve) || ve.Version != ProtocolVersion+1 {
		t.Fatalf("other version: got %v", err)
	}
}

func TestCleanEOFReturnsNil(t *testing.T) {
	req, err := ReadRequest(bytes.NewReader(nil))
	if req != nil || err != nil {
		t.Fatalf("expected nil, nil; got %v %v", req, err)
	}
	if _, err := ReadRequest(bytes.NewReader([]byte{0x50, 0x44})); err == nil {
		t.Fatal("expected truncated header error")
	}
}

// Shutdown is {"force":true} on the wire; CLIs of other builds send exactly
// this frame.
func TestManagementShutdownGoldenFrame(t *testing.T) {
	var buf bytes.Buffer
	if err := WriteManagementRequest(&buf, &ManagementRequest{Shutdown: &ManagementShutdown{Force: true}}); err != nil {
		t.Fatal(err)
	}
	payload := `{"force":true}`
	want := append([]byte{
		0x50, 0x44, 0x47, 0x41, // magic
		0x00, 0x00, // management version 0
		0x22, 0x4e, // kind 20002
		0, 0, 0, 0,
		byte(len(payload)), 0, 0, 0,
	}, payload...)
	if !bytes.Equal(buf.Bytes(), want) {
		t.Fatalf("got  % x\nwant % x", buf.Bytes(), want)
	}
	req, mgmt, err := ReadIncoming(&buf)
	if err != nil || req != nil || mgmt == nil || mgmt.Shutdown == nil || !mgmt.Shutdown.Force {
		t.Fatalf("ReadIncoming = %v, %#v, %v", req, mgmt, err)
	}
}

func TestManagementStatusResponseRoundTrip(t *testing.T) {
	var buf bytes.Buffer
	status := &ManagementStatus{DaemonVersion: "0.1.0", ProtocolVersion: ProtocolVersion, PID: 42, Root: "/r", Socket: "/r/agtd.sock", RunningSessions: true, Remote: "100.64.0.5:7433"}
	if err := WriteManagementResponse(&buf, &ManagementResponse{Status: status}); err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(buf.Bytes(), []byte(`"running_sessions":true`)) || !bytes.Contains(buf.Bytes(), []byte(`"remote":"100.64.0.5:7433"`)) || bytes.Contains(buf.Bytes(), []byte(`remote_error`)) {
		t.Fatalf("status payload uses unexpected field names: %s", buf.Bytes()[16:])
	}
	got, err := ReadManagementResponse(&buf)
	if err != nil || !reflect.DeepEqual(got.Status, status) {
		t.Fatalf("round trip = %#v, %v", got, err)
	}
}

func TestReadIncomingVersionMismatch(t *testing.T) {
	var buf bytes.Buffer
	if err := WriteErrorAtVersion(&buf, ProtocolVersion+1, "nope"); err != nil {
		t.Fatal(err)
	}
	_, _, err := ReadIncoming(&buf)
	var ve *VersionError
	if !errors.As(err, &ve) || ve.Version != ProtocolVersion+1 {
		t.Fatalf("got %v", err)
	}
	// The error frame itself decodes at its own version.
	buf.Reset()
	WriteErrorAtVersion(&buf, ProtocolVersion, "nope")
	resp, err := ReadResponse(&buf)
	if err != nil || resp.Error == nil || resp.Error.Message != "nope" {
		t.Fatalf("got %#v, %v", resp, err)
	}
}

// The frames below pin the encoding byte for byte: a change here breaks
// every CLI and daemon of another build, so it needs a protocol version
// bump.
func assertRequestGolden(t *testing.T, req *Request, golden []byte) {
	t.Helper()
	var buf bytes.Buffer
	if err := WriteRequest(&buf, req); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(buf.Bytes(), golden) {
		t.Fatalf("encoding changed\n got % x\nwant % x", buf.Bytes(), golden)
	}
	got, err := ReadRequest(bytes.NewReader(golden))
	if err != nil || !reflect.DeepEqual(got, req) {
		t.Fatalf("decode = %#v, %v", got, err)
	}
}

func assertResponseGolden(t *testing.T, resp *Response, golden []byte) {
	t.Helper()
	var buf bytes.Buffer
	if err := WriteResponse(&buf, resp); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(buf.Bytes(), golden) {
		t.Fatalf("encoding changed\n got % x\nwant % x", buf.Bytes(), golden)
	}
	got, err := ReadResponse(bytes.NewReader(golden))
	if err != nil || !reflect.DeepEqual(got, resp) {
		t.Fatalf("decode = %#v, %v", got, err)
	}
}

func TestSharedGoldenFrames(t *testing.T) {
	assertRequestGolden(t, &Request{CreateSession: &CreateSession{Cwd: "/w", Name: strp("fix"), Agent: "codex", Workspace: strp("ws")}}, []byte{
		0x50, 0x44, 0x47, 0x41, 0x01, 0x00, 0x03, 0x00, 0x00, 0x00, 0x00, 0x00,
		0x1f, 0x00, 0x00, 0x00, 0x02, 0x00, 0x00, 0x00, 0x2f, 0x77, 0x01, 0x03,
		0x00, 0x00, 0x00, 0x66, 0x69, 0x78, 0x05, 0x00, 0x00, 0x00, 0x63, 0x6f,
		0x64, 0x65, 0x78, 0x00, 0x01, 0x02, 0x00, 0x00, 0x00, 0x77, 0x73,
	})
	assertRequestGolden(t, &Request{KillSession: &KillSession{SessionID: "ab", Remove: true}}, []byte{
		0x50, 0x44, 0x47, 0x41, 0x01, 0x00, 0x04, 0x00, 0x00, 0x00, 0x00, 0x00,
		0x07, 0x00, 0x00, 0x00, 0x02, 0x00, 0x00, 0x00, 0x61, 0x62, 0x01,
	})
	assertResponseGolden(t, &Response{CreateSession: &session.CreateResult{SessionID: "ab", Cwd: "/w", Status: session.StatusRunning, Mode: session.ModePlan}}, []byte{
		0x50, 0x44, 0x47, 0x41, 0x01, 0x00, 0x66, 0x00, 0x00, 0x00, 0x00, 0x00,
		0x0e, 0x00, 0x00, 0x00, 0x02, 0x00, 0x00, 0x00, 0x61, 0x62, 0x02, 0x00,
		0x00, 0x00, 0x2f, 0x77, 0x02, 0x02,
	})
	assertResponseGolden(t, &Response{SessionEnded: &SessionEnded{SessionID: "ab", Status: session.StatusExited, ExitCode: i32p(-1), Error: strp("e")}}, []byte{
		0x50, 0x44, 0x47, 0x41, 0x01, 0x00, 0x6a, 0x00, 0x00, 0x00, 0x00, 0x00,
		0x12, 0x00, 0x00, 0x00, 0x02, 0x00, 0x00, 0x00, 0x61, 0x62, 0x03, 0x01,
		0xff, 0xff, 0xff, 0xff, 0x01, 0x01, 0x00, 0x00, 0x00, 0x65,
	})
	assertResponseGolden(t, ErrorResponsef("no"), []byte{
		0x50, 0x44, 0x47, 0x41, 0x01, 0x00, 0x72, 0x00, 0x00, 0x00, 0x00, 0x00,
		0x06, 0x00, 0x00, 0x00, 0x02, 0x00, 0x00, 0x00, 0x6e, 0x6f,
	})
	created := time.Unix(1_700_000_000, 123_456_789).UTC()
	exited := time.Unix(1_700_000_100, 0).UTC()
	assertResponseGolden(t, &Response{Session: &session.Record{
		SessionID: "ab", Agent: "sh", Model: strp("m"), Mode: session.ModeExecute, Cwd: "/w",
		Status: session.StatusExited, WorkerPID: u32p(7), ExitCode: i32p(3),
		Attention: session.AttentionNotice, AttentionSummary: strp("s"),
		CreatedAt: created, UpdatedAt: created, ExitedAt: &exited, Workspace: strp("ws"),
	}}, []byte{
		0x50, 0x44, 0x47, 0x41, 0x01, 0x00, 0x6c, 0x00, 0x00, 0x00, 0x00, 0x00,
		0x59, 0x00, 0x00, 0x00, 0x02, 0x00, 0x00, 0x00, 0x61, 0x62, 0x02, 0x00,
		0x00, 0x00, 0x73, 0x68, 0x01, 0x01, 0x00, 0x00, 0x00, 0x6d, 0x01, 0x02,
		0x00, 0x00, 0x00, 0x2f, 0x77, 0x03, 0x01, 0x07, 0x00, 0x00, 0x00, 0x00,
		0x01, 0x03, 0x00, 0x00, 0x00, 0x00, 0x02, 0x01, 0x01, 0x00, 0x00, 0x00,
		0x73, 0x00, 0xf1, 0x53, 0x65, 0x00, 0x00, 0x00, 0x00, 0x15, 0xcd, 0x5b,
		0x07, 0x00, 0xf1, 0x53, 0x65, 0x00, 0x00, 0x00, 0x00, 0x15, 0xcd, 0x5b,
		0x07, 0x01, 0x64, 0xf1, 0x53, 0x65, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
		0x00, 0x00, 0x01, 0x02, 0x00, 0x00, 0x00, 0x77, 0x73,
	})
	assertRequestGolden(t, &Request{AddWorkspace: &AddWorkspace{Name: "m", Path: "/w"}}, []byte{
		0x50, 0x44, 0x47, 0x41, 0x01, 0x00, 0x11, 0x00, 0x00, 0x00, 0x00, 0x00,
		0x0b, 0x00, 0x00, 0x00, 0x01, 0x00, 0x00, 0x00, 0x6d, 0x02, 0x00, 0x00,
		0x00, 0x2f, 0x77,
	})
	assertRequestGolden(t, &Request{RemoveWorkspace: &WorkspaceRef{Name: "m"}}, []byte{
		0x50, 0x44, 0x47, 0x41, 0x01, 0x00, 0x12, 0x00, 0x00, 0x00, 0x00, 0x00,
		0x05, 0x00, 0x00, 0x00, 0x01, 0x00, 0x00, 0x00, 0x6d,
	})
	assertResponseGolden(t, &Response{Workspaces: &[]session.Workspace{{Name: "m", Path: "/w", CreatedAt: created}}}, []byte{
		0x50, 0x44, 0x47, 0x41, 0x01, 0x00, 0x74, 0x00, 0x00, 0x00, 0x00, 0x00,
		0x1b, 0x00, 0x00, 0x00, 0x01, 0x00, 0x00, 0x00, 0x01, 0x00, 0x00, 0x00,
		0x6d, 0x02, 0x00, 0x00, 0x00, 0x2f, 0x77, 0x00, 0xf1, 0x53, 0x65, 0x00,
		0x00, 0x00, 0x00, 0x15, 0xcd, 0x5b, 0x07,
	})
}

// The input field (CapAttachField): the request has no payload, and the
// answer's layout is pinned, Found first so a client can stop reading at
// a field that was not found.
func TestFieldGoldenFrames(t *testing.T) {
	assertRequestGolden(t, &Request{AttachField: Empty}, []byte{
		0x50, 0x44, 0x47, 0x41, 0x01, 0x00, 0x34, 0x00, 0x00, 0x00, 0x00, 0x00,
		0x00, 0x00, 0x00, 0x00,
	})
	assertResponseGolden(t, &Response{Field: &Field{Found: true, Cols: 120, Lines: 40, Alternate: true, Row: 36, Col: 2, Top: 35, BoxTop: 34, BoxBottom: 37, TextCol: 2, Width: 116, GrowsUp: true,
		Rows: []string{"hi", ""}, Style: []byte("\x1b[0m"), Chrome: []ScreenRow{{Index: 35, VT: []byte("\u2500")}}}}, []byte{
		0x50, 0x44, 0x47, 0x41, 0x01, 0x00, 0x98, 0x00, 0x00, 0x00, 0x00, 0x00,
		0x3a, 0x00, 0x00, 0x00, 0x01, 0x78, 0x00, 0x28, 0x00, 0x01, 0x24, 0x00,
		0x02, 0x00, 0x23, 0x00, 0x22, 0x00, 0x25, 0x00, 0x02, 0x00, 0x74, 0x00,
		0x01, 0x00, 0x00, 0x02, 0x00, 0x00, 0x00, 0x02, 0x00, 0x00, 0x00, 0x68,
		0x69, 0x00, 0x00, 0x00, 0x00, 0x04, 0x00, 0x00, 0x00, 0x1b, 0x5b, 0x30,
		0x6d, 0x01, 0x00, 0x00, 0x00, 0x23, 0x00, 0x03, 0x00, 0x00, 0x00, 0xe2,
		0x94, 0x80,
	})
	assertResponseGolden(t, &Response{Field: &Field{Cols: 80, Lines: 24}}, []byte{
		0x50, 0x44, 0x47, 0x41, 0x01, 0x00, 0x98, 0x00, 0x00, 0x00, 0x00, 0x00,
		0x23, 0x00, 0x00, 0x00, 0x00, 0x50, 0x00, 0x18, 0x00, 0x00, 0x00, 0x00,
		0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
		0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
		0x00, 0x00, 0x00,
	})
}

func TestOversizedFrameRejectedBeforeAllocating(t *testing.T) {
	var h [16]byte
	binary.LittleEndian.PutUint32(h[0:4], frameMagic)
	binary.LittleEndian.PutUint16(h[4:6], ProtocolVersion)
	binary.LittleEndian.PutUint16(h[6:8], uint16(kListSessionsRequest))
	binary.LittleEndian.PutUint32(h[12:16], 0xFFFFFFFF)
	if _, err := ReadRequest(bytes.NewReader(h[:])); err == nil || !strings.Contains(err.Error(), "too large") {
		t.Fatalf("want size error, got %v", err)
	}
	if err := WriteResponse(io.Discard, &Response{PtyOutput: &Bytes{Data: make([]byte, MaxFramePayload+1)}}); err == nil {
		t.Fatal("oversized write accepted")
	}
}

func TestReadIncomingDecodeError(t *testing.T) {
	var buf bytes.Buffer
	if err := writeFrame(&buf, ProtocolVersion, 99, nil); err != nil { // no such kind
		t.Fatal(err)
	}
	_, _, err := ReadIncoming(&buf)
	var de *DecodeError
	if !errors.As(err, &de) || de.Version != ProtocolVersion {
		t.Fatalf("got %v", err)
	}
}

func TestInvalidUTF8StringsAreRejected(t *testing.T) {
	var buf bytes.Buffer
	if err := WriteRequest(&buf, &Request{CreateSession: &CreateSession{Cwd: "/ok\xff", Agent: "sh"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadRequest(&buf); err == nil || !strings.Contains(err.Error(), "invalid UTF-8") {
		t.Fatalf("got %v", err)
	}
}

// The client terminal's colours ride in CreateSession (under the control
// stream's terminal-colors) and in AttachSession (as an attach feature);
// without the capability the frames are the ones older peers know.
func TestTerminalColorsFrames(t *testing.T) {
	colors := &TerminalColors{Foreground: RGB{0xdd, 0xdd, 0xdd}, Background: RGB{0x1e, 0x1e, 0x2e}}
	create := &Request{CreateSession: &CreateSession{Cwd: "/w", Agent: "codex", Token: "tok", Colors: colors}}
	var buf bytes.Buffer
	if err := WriteRequestWith(&buf, allFeatures(), create); err != nil {
		t.Fatal(err)
	}
	want := []byte{
		0x50, 0x44, 0x47, 0x41, 0x01, 0x00, 0x03, 0x00, 0x00, 0x00, 0x00, 0x00,
		0x20, 0x00, 0x00, 0x00, 0x02, 0x00, 0x00, 0x00, 0x2f, 0x77, 0x00, 0x05,
		0x00, 0x00, 0x00, 0x63, 0x6f, 0x64, 0x65, 0x78, 0x00, 0x00, 0x03, 0x00,
		0x00, 0x00, 0x74, 0x6f, 0x6b, // token
		0x01, 0xdd, 0xdd, 0xdd, 0x1e, 0x1e, 0x2e, // colours: present, fg, bg
	}
	if !bytes.Equal(buf.Bytes(), want) {
		t.Fatalf("create encoding changed\n got % x\nwant % x", buf.Bytes(), want)
	}
	if got, err := ReadRequestWith(bytes.NewReader(want), allFeatures()); err != nil || !reflect.DeepEqual(got, create) {
		t.Fatalf("create decode = %#v, %v", got, err)
	}
	// An older daemon never sees the colours.
	buf.Reset()
	if err := WriteRequest(&buf, create); err != nil {
		t.Fatal(err)
	}
	if got, err := ReadRequest(bytes.NewReader(buf.Bytes())); err != nil || got.CreateSession.Colors != nil {
		t.Fatalf("create without the capability = %#v, %v", got, err)
	}

	attach := &Request{AttachSession: &AttachSession{SessionID: "ab", Kind: session.AttachmentAttach, Geometry: Geometry{Cols: 80, Rows: 24}, Features: []string{CapTerminalColors}, Colors: colors}}
	assertRequestGolden(t, attach, []byte{
		0x50, 0x44, 0x47, 0x41, 0x01, 0x00, 0x05, 0x00, 0x00, 0x00, 0x00, 0x00,
		0x2d, 0x00, 0x00, 0x00, 0x02, 0x00, 0x00, 0x00, 0x61, 0x62, 0x01, 0x50,
		0x00, 0x18, 0x00, 0x00, 0x00, 0x00, 0x00, 0x01, 0x00, 0x00, 0x00, 0x0f,
		0x00, 0x00, 0x00, 0x74, 0x65, 0x72, 0x6d, 0x69, 0x6e, 0x61, 0x6c, 0x2d,
		0x63, 0x6f, 0x6c, 0x6f, 0x72, 0x73, // "terminal-colors"
		0x01, 0xdd, 0xdd, 0xdd, 0x1e, 0x1e, 0x2e,
	})
	roundTripRequest(t, &Request{AttachSession: &AttachSession{SessionID: "ab", Kind: session.AttachmentAttach, Features: []string{CapTerminalColors}}})

	if got := colors.String(); got != "dddddd/1e1e2e" {
		t.Fatalf("String() = %q", got)
	}
	if got, err := ParseTerminalColors("dddddd/1e1e2e"); err != nil || *got != *colors {
		t.Fatalf("ParseTerminalColors = %+v, %v", got, err)
	}
	for _, bad := range []string{"", "dddddd", "dddddd/1e1e2", "gggggg/1e1e2e", "dddddd/1e1e2e/"} {
		if _, err := ParseTerminalColors(bad); err == nil {
			t.Fatalf("ParseTerminalColors(%q) accepted", bad)
		}
	}
	if (*TerminalColors)(nil).String() != "" {
		t.Fatal("nil String()")
	}
}

// Frames and their acknowledgements (attach-frames) are pinned byte for
// byte; empty byte fields and row lists round-trip as nil.
func TestFrameGoldenFrames(t *testing.T) {
	f := &Frame{Seq: 7, Full: true, Cols: 80, Lines: 24, Scrolled: 1, CursorCol: 2, CursorShape: 6,
		Prefix: []byte("\x1b[?2004h\x1b[H"), Keyboard: []byte("\x1b[=7;1u"), Effects: []byte("\a"),
		Field: &Field{Found: true, Cols: 80, Lines: 24, Col: 2, TextCol: 2, Width: 78, Rows: []string{""}},
		Rows:  []FrameRow{{Index: 0, VT: []byte("$ ")}, {Index: 2}}, History: [][]byte{[]byte("old")}}
	assertResponseGolden(t, &Response{AttachFrame: f}, []byte{
		0x50, 0x44, 0x47, 0x41, 0x01, 0x00, 0x99, 0x00, 0x00, 0x00, 0x00, 0x00,
		0x79, 0x00, 0x00, 0x00, // payload length
		0x07, 0x00, 0x00, 0x00, 0x01, 0x00, 0x50, 0x00, 0x18, 0x00, 0x01, 0x00, // seq, full, alternate, cols, lines, scrolled
		0x00, 0x00, 0x02, 0x00, 0x06, // cursor row, col, shape
		0x0b, 0x00, 0x00, 0x00, 0x1b, 0x5b, 0x3f, 0x32, 0x30, 0x30, 0x34, 0x68, 0x1b, 0x5b, 0x48, // prefix
		0x07, 0x00, 0x00, 0x00, 0x1b, 0x5b, 0x3d, 0x37, 0x3b, 0x31, 0x75, // keyboard
		0x00, 0x00, 0x00, 0x00, // colours
		0x01, 0x00, 0x00, 0x00, 0x07, // effects
		0x01, // field present
		0x01, 0x50, 0x00, 0x18, 0x00, 0x00, 0x00, 0x00, 0x02, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x02, 0x00, 0x4e, 0x00,
		0x00, 0x00, 0x00, 0x01, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, // field
		0x02, 0x00, 0x00, 0x00, // rows
		0x00, 0x00, 0x02, 0x00, 0x00, 0x00, 0x24, 0x20, // row 0 "$ "
		0x02, 0x00, 0x00, 0x00, 0x00, 0x00, // row 2, blank
		0x01, 0x00, 0x00, 0x00, 0x03, 0x00, 0x00, 0x00, 0x6f, 0x6c, 0x64, // history: one row, "old"
	})
	assertRequestGolden(t, &Request{AttachFrameAck: &FrameAck{Seq: 7, Field: true}}, []byte{
		0x50, 0x44, 0x47, 0x41, 0x01, 0x00, 0x35, 0x00, 0x00, 0x00, 0x00, 0x00,
		0x05, 0x00, 0x00, 0x00, 0x07, 0x00, 0x00, 0x00, 0x01,
	})
	roundTripResponse(t, &Response{AttachFrame: &Frame{Seq: 1, Cols: 10, Lines: 2}})
	roundTripRequest(t, &Request{AttachFrameAck: &FrameAck{Seq: 9}})
}
