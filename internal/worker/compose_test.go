package worker

import (
	"testing"

	"agtd.sh/internal/protocol"
)

// compose follows the terminal's modes: a plain carriage return for Enter
// and the text as it is by default; bracketed paste markers once the
// program turned that on; CSI u for Enter once the program asked for every
// key as an escape code; newlines as carriage returns unbracketed.
func TestCompose(t *testing.T) {
	src, err := newTerminalState(80, 24, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	s := newOwnerState("compose", nil, src)
	got := func(text string) string {
		t.Helper()
		out, err := s.compose([]byte(text))
		if err != nil {
			t.Fatal(err)
		}
		return string(out)
	}
	if g := got(""); g != "\r" {
		t.Fatalf("Enter alone = %q", g)
	}
	if g := got("run the tests"); g != "run the tests\r" {
		t.Fatalf("plain = %q", g)
	}
	if g := got("one\ntwo"); g != "one\rtwo\r" {
		t.Fatalf("newline unbracketed = %q", g)
	}
	if err := s.publishScanned([]byte("\x1b[?2004h")); err != nil {
		t.Fatal(err)
	}
	if g := got("one\ntwo"); g != "\x1b[200~one\ntwo\x1b[201~\r" {
		t.Fatalf("bracketed = %q", g)
	}
	// The kitty keyboard protocol's disambiguate flag leaves Enter as it
	// is; report-all-keys makes it CSI u.
	if err := s.publishScanned([]byte("\x1b[>1u")); err != nil {
		t.Fatal(err)
	}
	if g := got(""); g != "\r" {
		t.Fatalf("Enter under disambiguate = %q", g)
	}
	if err := s.publishScanned([]byte("\x1b[>8u")); err != nil {
		t.Fatal(err)
	}
	if g := got("hi"); g != "\x1b[200~hi\x1b[201~\x1b[13u" {
		t.Fatalf("Enter under report-all-keys = %q", g)
	}
	if err := s.publishScanned([]byte("\x1b[<u\x1b[<u\x1b[?2004l")); err != nil {
		t.Fatal(err)
	}
	if g := got("hi"); g != "hi\r" {
		t.Fatalf("back to defaults = %q", g)
	}
}

// Through the worker: SendInput with Compose from a script, and
// AttachCompose on an attach stream that asked for the capability, each
// reach the program as the text and Enter; an attach stream without the
// capability does not get AttachCompose.
func TestComposeThroughTheWorker(t *testing.T) {
	h := startWorker(t)
	c, attached := h.attachWith(protocol.AttachSession{Features: []string{protocol.CapAttachCompose}})
	if !attached.HasFeature(protocol.CapAttachCompose) {
		t.Fatalf("compose not negotiated: %v", attached.Features)
	}
	// "ready" may already be on the screen (in the snapshot) or still on
	// its way; the input queues in the PTY either way.
	if resp := h.request(&protocol.Request{SendInput: &protocol.SendInput{SessionID: h.sessionID, Data: []byte("from a script"), Compose: true}}); resp.InputAccepted == nil {
		t.Fatalf("send-input --compose: %+v", resp)
	}
	c.expectOutput("got:from a script")
	c.send(&protocol.Request{AttachCompose: &protocol.Bytes{Data: []byte("from the overlay")}})
	c.expectOutput("got:from the overlay")
	// Enter alone submits what was typed before it, in order.
	c.input("typed")
	c.send(&protocol.Request{AttachCompose: &protocol.Bytes{}})
	c.expectOutput("got:typed")
	c.input("done\n")
	c.expectEnd()
	h.waitExit()
}
