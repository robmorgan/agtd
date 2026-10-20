package cli

import (
	"reflect"
	"strings"
	"testing"
)

func data(s string) attachAction { return attachAction{kind: actionData, data: []byte(s)} }

func TestAttachParser(t *testing.T) {
	for _, tc := range []struct {
		name   string
		chunks []string
		want   []attachAction
	}{
		{"regular bytes", []string{"hello"}, []attachAction{data("hello")}},
		{"mouse and scroll", []string{"\x1b[<64;10;5M"}, []attachAction{data("\x1b[<64;10;5M")}},
		{"kitty ctrl-[", []string{"\x1b[91;5u"}, []attachAction{{kind: actionPreviousSession}}},
		{"kitty ctrl-\\", []string{"\x1b[92;5u"}, []attachAction{{kind: actionDetach}}},
		{"kitty ctrl-]", []string{"\x1b[93;5u"}, []attachAction{{kind: actionNextSession}}},
		{"kitty ctrl-y", []string{"\x1b[121;5u"}, []attachAction{{kind: actionOpenOverlay}}},
		{"kitty with lock modifiers and alternates", []string{"\x1b[93:125;69u"}, []attachAction{{kind: actionNextSession}}},
		{"kitty repeat", []string{"\x1b[92;5:2u"}, []attachAction{{kind: actionDetach}}},
		{"kitty release is not a hotkey", []string{"\x1b[93;5:3u"}, []attachAction{data("\x1b[93;5:3u")}},
		{"kitty with other modifiers is not a hotkey", []string{"\x1b[92;7u"}, []attachAction{data("\x1b[92;7u")}},
		{"ctrl-y byte", []string{"\x19"}, []attachAction{{kind: actionOpenOverlay}}},
		{"ctrl-\\ and ctrl-] bytes", []string{"\x1c", "\x1d"}, []attachAction{{kind: actionDetach}, {kind: actionNextSession}}},
		{"split sequence", []string{"\x1b[91;", "5u"}, []attachAction{{kind: actionPreviousSession}}},
		{"order is kept", []string{"ab\x1ccd\x1d"}, []attachAction{data("ab"), {kind: actionDetach}, data("cd"), {kind: actionNextSession}}},
	} {
		var p attachParser
		var got []attachAction
		for _, chunk := range tc.chunks {
			got = append(got, p.push([]byte(chunk))...)
		}
		if !reflect.DeepEqual(got, tc.want) {
			t.Errorf("%s: got %+v, want %+v", tc.name, got, tc.want)
		}
	}
}

func TestKeyDecoder(t *testing.T) {
	for _, tc := range []struct {
		name   string
		chunks []string
		want   []keyEvent
	}{
		{"text", []string{"aB"}, []keyEvent{char('a'), char('B')}},
		{"utf-8 split across reads", []string{"\xc3", "\xa9"}, []keyEvent{char('é')}},
		{"legacy keys", []string{"\r\x7f\t"}, []keyEvent{key(keyEnter), key(keyBackspace), key(keyTab)}},
		{"lone esc", []string{"\x1b"}, []keyEvent{key(keyEsc)}},
		{"arrows", []string{"\x1b[A\x1b[B\x1bOA"}, []keyEvent{key(keyUp), key(keyDown), key(keyUp)}},
		{"pages", []string{"\x1b[5~\x1b[6~"}, []keyEvent{key(keyPageUp), key(keyPageDown)}},
		{"modified arrow", []string{"\x1b[1;5A"}, []keyEvent{{code: keyUp, mods: modCtrl}}},
		{"ctrl letter", []string{"\x19"}, []keyEvent{{code: keyChar, r: 'y', mods: modCtrl}}},
		{"alt letter", []string{"\x1bx"}, []keyEvent{{code: keyChar, r: 'x', mods: modAlt}}},
		{"kitty esc", []string{"\x1b[27u"}, []keyEvent{key(keyEsc)}},
		{"kitty enter and ctrl-y", []string{"\x1b[13u\x1b[121;5u"}, []keyEvent{key(keyEnter), {code: keyChar, r: 'y', mods: modCtrl}}},
		{"kitty release", []string{"\x1b[27;1:3u"}, []keyEvent{{release: true}}},
		{"csi split across reads", []string{"\x1b[", "B"}, []keyEvent{key(keyDown)}},
		{"bracketed paste", []string{"\x1b[200~he", "llo\x1b[201~x"}, []keyEvent{{isPaste: true, paste: "hello"}, char('x')}},
	} {
		var d keyDecoder
		var got []keyEvent
		var raw []byte
		for _, chunk := range tc.chunks {
			for _, ev := range d.push([]byte(chunk)) {
				// Every event carries the bytes it came from, in order.
				raw = append(raw, ev.raw...)
				ev.raw = nil
				got = append(got, ev)
			}
		}
		if !reflect.DeepEqual(got, tc.want) {
			t.Errorf("%s: got %+v, want %+v", tc.name, got, tc.want)
		}
		if all := strings.Join(tc.chunks, ""); tc.want != nil && !strings.Contains(all, string(raw)) {
			t.Errorf("%s: raw bytes %q are not from the input %q", tc.name, raw, all)
		}
	}
}

// Sequences the decoder does not know still carry their bytes, wherever a
// read splits them: the local echo forwards every byte it is given.
func TestKeyDecoderKeepsEveryByte(t *testing.T) {
	for _, in := range []string{
		"\x1b[15~",                         // F5
		"\x1bOP",                           // F1
		"\x1b[2~",                          // Insert
		"\x1b[<0;10;5M\x1b[<0;10;5m",       // SGR mouse press and release
		"\x1b[M !!",                        // X10 mouse
		"\x1b[97;1:3u",                     // kitty key release
		"\x1b[57399u",                      // kitty keypad 0
		"\x1b[12;5R",                       // cursor position report
		"\x1b[?62;22c",                     // primary device attributes reply
		"\x1b[?1u",                         // kitty keyboard flags reply
		"\x1b[I\x1b[O",                     // focus in and out
		"\x1b]11;rgb:0000/0000/0000\x1b\\", // OSC reply
		"\x1b[\x01x",                       // not a CSI sequence after all
		"\x1b[200~pasted\x1b[201~",
	} {
		for split := 0; split <= len(in); split++ {
			var d keyDecoder
			var raw []byte
			for _, chunk := range []string{in[:split], in[split:]} {
				for _, ev := range d.push([]byte(chunk)) {
					raw = append(raw, ev.raw...)
				}
			}
			if string(raw) != in {
				t.Errorf("%q split at %d: forwarded %q", in, split, raw)
			}
		}
	}
	// Kitty functional keys are not text.
	var d keyDecoder
	if evs := d.push([]byte("\x1b[57399u")); len(evs) != 1 || evs[0].code != keyNone {
		t.Errorf("keypad 0: %+v", evs)
	}
}

// pushAll feeds the decoder chunks and returns the events' bytes, checking
// after each push that what it holds stays bounded.
func pushAll(t *testing.T, d *keyDecoder, chunks []string) (raw []byte, events []keyEvent) {
	t.Helper()
	for _, chunk := range chunks {
		evs := d.push([]byte(chunk))
		for _, ev := range evs {
			raw = append(raw, ev.raw...)
		}
		events = append(events, evs...)
		if held := len(d.pending) + len(d.paste); held > pasteMax+len(chunk) {
			t.Fatalf("holds %d bytes", held)
		}
	}
	return raw, events
}

func chunked(s string, n int) []string {
	var out []string
	for len(s) > n {
		out = append(out, s[:n])
		s = s[n:]
	}
	return append(out, s)
}

// A paste longer than pasteMax is forwarded as it arrives, unpredicted,
// whichever read splits its end marker; a shorter one is one event.
func TestKeyDecoderStreamsLongPastes(t *testing.T) {
	long := string(pasteStart) + strings.Repeat("0123456789abcdef", (4<<20)/16) + string(pasteEnd)
	var d keyDecoder
	raw, events := pushAll(t, &d, chunked(long+"x", 4096))
	if string(raw) != long+"x" {
		t.Fatalf("forwarded %d bytes of %d", len(raw), len(long)+1)
	}
	for _, ev := range events[:len(events)-1] {
		if ev.isPaste || ev.code != keyNone {
			t.Fatalf("a long paste was held for prediction: %+v", ev.code)
		}
	}
	if last := events[len(events)-1]; last.code != keyChar || last.r != 'x' || d.paste != nil {
		t.Fatalf("after the paste: %+v, still pasting %v", last, d.paste != nil)
	}

	for _, n := range []int{pasteMax / 2, pasteMax + 10} {
		body := strings.Repeat("p", n)
		in := string(pasteStart) + body + string(pasteEnd) + "x"
		end := len(pasteStart) + n
		for split := end - 1; split <= end+len(pasteEnd); split++ {
			var d keyDecoder
			raw, events := pushAll(t, &d, []string{in[:split], in[split:]})
			if string(raw) != in {
				t.Fatalf("%d bytes, split at %d: forwarded %d bytes", n, split, len(raw))
			}
			last := events[len(events)-1]
			if last.r != 'x' {
				t.Fatalf("%d bytes, split at %d: the end marker was missed", n, split)
			}
			if n < pasteMax && (len(events) != 2 || !events[0].isPaste || events[0].paste != body) {
				t.Fatalf("%d bytes, split at %d: not one paste: %d events", n, split, len(events))
			}
		}
	}
}

// An incomplete sequence longer than any key is not one, and what nothing
// completes is given up by expire.
func TestKeyDecoderBoundsIncompleteSequences(t *testing.T) {
	long := "\x1b[" + strings.Repeat("1;", 200)
	var d keyDecoder
	if raw, _ := pushAll(t, &d, chunked(long, 50)); string(raw) != long || d.held() {
		t.Fatalf("forwarded %q, holding %v", raw, d.held())
	}

	d = keyDecoder{}
	if evs := d.push([]byte("\x1b[")); evs != nil || !d.held() {
		t.Fatalf("Alt-[: %+v", evs)
	}
	if evs := d.expire(); len(evs) != 1 || string(evs[0].raw) != "\x1b[" || d.held() {
		t.Fatalf("expired %+v", evs)
	}

	// A paste whose end is late is forwarded from then on.
	d = keyDecoder{}
	var raw []byte
	for _, ev := range d.push([]byte("\x1b[200~abc")) {
		raw = append(raw, ev.raw...)
	}
	for _, ev := range d.expire() {
		raw = append(raw, ev.raw...)
	}
	if d.held() {
		t.Fatal("still held after expiring")
	}
	evs := d.push([]byte("def\x1b[201~x"))
	for _, ev := range evs {
		raw = append(raw, ev.raw...)
	}
	if string(raw) != "\x1b[200~abcdef\x1b[201~x" || evs[len(evs)-1].r != 'x' {
		t.Fatalf("forwarded %q", raw)
	}
}
