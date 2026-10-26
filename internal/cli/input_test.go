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
		{"kitty release dropped", []string{"\x1b[27;1:3u"}, nil},
		{"csi split across reads", []string{"\x1b[", "B"}, []keyEvent{key(keyDown)}},
		{"bracketed paste", []string{"\x1b[200~he", "llo\x1b[201~x"}, []keyEvent{{isPaste: true, paste: "hello"}, char('x')}},
		// The inbox and the overlay never see mouse reports, focus
		// changes or keys they have no name for (the echo does).
		{"mouse and focus dropped", []string{"\x1b[<64;10;5M\x1b[M`*%\x1b[I"}, nil},
		{"unnamed keys dropped", []string{"\x1bOP\x1b[15~"}, nil},
		{"query replies dropped", []string{"\x1b[12;40R\x1b[?62;22c\x1b[?1u"}, nil},
		{"x10 mouse split across reads", []string{"\x1b[M`", "*%a"}, []keyEvent{char('a')}},
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
			d := keyDecoder{forwardAll: true}
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
	d := keyDecoder{forwardAll: true}
	if evs := d.push([]byte("\x1b[57399u")); len(evs) != 1 || evs[0].code != keyUnknown {
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
	d := keyDecoder{forwardAll: true}
	raw, events := pushAll(t, &d, chunked(long+"x", 4096))
	if string(raw) != long+"x" {
		t.Fatalf("forwarded %d bytes of %d", len(raw), len(long)+1)
	}
	for _, ev := range events[:len(events)-1] {
		if ev.isPaste || ev.code != keyUnknown {
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
			d := keyDecoder{forwardAll: true}
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
	d := keyDecoder{forwardAll: true}
	if raw, _ := pushAll(t, &d, chunked(long, 50)); string(raw) != long || d.held() {
		t.Fatalf("forwarded %q, holding %v", raw, d.held())
	}

	d = keyDecoder{forwardAll: true}
	if evs := d.push([]byte("\x1b[")); evs != nil || !d.held() {
		t.Fatalf("Alt-[: %+v", evs)
	}
	if evs := d.expire(); len(evs) != 1 || string(evs[0].raw) != "\x1b[" || d.held() {
		t.Fatalf("expired %+v", evs)
	}

	// A paste whose end is late is forwarded from then on.
	d = keyDecoder{forwardAll: true}
	var raw []byte
	collect := func(evs []keyEvent) []keyEvent {
		for _, ev := range evs {
			raw = append(raw, ev.raw...)
		}
		return evs
	}
	collect(d.push([]byte("\x1b[200~abc")))
	collect(d.expire())
	if !d.held() {
		t.Fatal("no longer in the paste after the first expiry")
	}
	evs := collect(d.push([]byte("def\x1b[201~x")))
	if string(raw) != "\x1b[200~abcdef\x1b[201~x" || evs[len(evs)-1].r != 'x' {
		t.Fatalf("forwarded %q", raw)
	}

	// A paste whose end never comes is given up at the next expiry, and
	// what follows is keys again.
	d, raw = keyDecoder{forwardAll: true}, nil
	collect(d.push([]byte("\x1b[200~abc")))
	collect(d.expire())
	collect(d.expire())
	if d.held() || string(raw) != "\x1b[200~abc" {
		t.Fatalf("forwarded %q, holding %v", raw, d.held())
	}
	if evs := d.push([]byte("x")); len(evs) != 1 || evs[0].r != 'x' {
		t.Fatalf("after the paste: %+v", evs)
	}
}

// A long paste whose end never comes is released in full by expire, the
// bytes that might have begun its end marker included.
func TestKeyDecoderReleasesAnUnendedPaste(t *testing.T) {
	in := string(pasteStart) + strings.Repeat("x", pasteMax+10)
	d := keyDecoder{forwardAll: true}
	raw, _ := pushAll(t, &d, chunked(in, 4096))
	for _, ev := range d.expire() {
		raw = append(raw, ev.raw...)
	}
	if string(raw) != in || d.held() {
		t.Fatalf("forwarded %d of %d bytes, holding %v", len(raw), len(in), d.held())
	}
}

// Wheel scrolls and motion edit nothing; clicks, drags' presses and the
// extra buttons may (they can move the cursor or pick from a menu).
func TestMouseKeyButtons(t *testing.T) {
	for button, want := range map[int]keyCode{
		-1:  keyUnknown, // an X10 byte below 32
		0:   keyUnknown, // left press
		2:   keyUnknown, // right press
		3:   keyUnknown, // release (X10)
		32:  keyPassive, // drag with the left button
		35:  keyPassive, // motion, no button
		64:  keyPassive, // wheel up
		65:  keyPassive, // wheel down
		68:  keyPassive, // Shift-wheel
		80:  keyPassive, // Ctrl-wheel
		128: keyUnknown, // button 8
	} {
		if got := mouseKey(button); got != want {
			t.Errorf("button %d: %v, want %v", button, got, want)
		}
	}
}

// Value: protects=with holdEsc, a paste start marker split after its ESC is still a paste, a lone ESC becomes Esc only on expire, and an unterminated paste is given up so keys work again; fails_when=the split ESC decodes as Esc, or a paste swallows keys forever; why_new=the inbox now enables bracketed paste; seam=none; no_claim=the inbox's timing
// A decoder that holds a lone ESC: a split paste marker stays a paste, a
// lone ESC is Esc once nothing follows, and a paste without its end is
// given up so keys work again.
func TestKeyDecoderHoldsALoneEsc(t *testing.T) {
	d := keyDecoder{holdEsc: true}
	if evs := d.push([]byte("\x1b")); len(evs) != 0 || !d.held() {
		t.Fatalf("lone esc: %+v held %v", evs, d.held())
	}
	evs := d.push([]byte("[200~\t2\r\x1b[201~"))
	if len(evs) != 1 || !evs[0].isPaste || evs[0].paste != "\t2\r" {
		t.Fatalf("split paste: %+v", evs)
	}

	d = keyDecoder{holdEsc: true}
	d.push([]byte("\x1b"))
	if evs := d.expire(); len(evs) != 1 || evs[0].code != keyEsc || d.held() {
		t.Fatalf("expired esc: %+v held %v", evs, d.held())
	}
	if evs := d.push([]byte("\x1b[A")); len(evs) != 1 || evs[0].code != keyUp {
		t.Fatalf("arrow after: %+v", evs)
	}

	d = keyDecoder{holdEsc: true}
	if evs := d.push([]byte("\x1b[200~half a paste")); len(evs) != 0 || !d.pasting() {
		t.Fatalf("open paste: %+v", evs)
	}
	for _, ev := range append(d.expire(), d.expire()...) {
		if ev.code != keyUnknown {
			t.Fatalf("giving up a paste made a key: %+v", ev)
		}
	}
	// What follows a given-up paste is the rest of it: dropped, never
	// keys, up to its end marker (or the next expire).
	if evs := d.push([]byte("\t2\r")); len(evs) != 0 || !d.pasting() {
		t.Fatalf("rest of a given-up paste: %+v", evs)
	}
	if evs := d.push([]byte("y\x1b[20")); len(evs) != 0 {
		t.Fatalf("rest of a given-up paste, end marker split: %+v", evs)
	}
	if evs := d.push([]byte("1~q")); d.held() || len(evs) != 1 || evs[0].code != keyChar || evs[0].r != 'q' {
		t.Fatalf("keys after the end of a given-up paste: %+v held %v", evs, d.held())
	}
	d = keyDecoder{holdEsc: true}
	d.push([]byte("\x1b[200~half a paste"))
	d.expire()
	d.expire()
	d.push([]byte("more of it"))
	if evs := append(d.expire(), d.push([]byte("q"))...); len(evs) != 1 || evs[0].r != 'q' {
		t.Fatalf("keys after a quiet expire: %+v", evs)
	}
	// A decoder that forwards raw bytes (attach) decodes what follows.
	d = keyDecoder{forwardAll: true}
	d.push([]byte("\x1b[200~half a paste"))
	d.expire()
	d.expire()
	if evs := d.push([]byte("q")); len(evs) != 1 || evs[0].r != 'q' {
		t.Fatalf("forwarding decoder after a given-up paste: %+v", evs)
	}

	// Value: protects=a held ESC that the next read does not continue is Esc followed by that read's keys, not Alt-key; fails_when=push joins the held ESC to the next read; why_new=holdEsc only met reads that continue a sequence; seam=none; no_claim=Alt and a non-printable key split across reads (taken as Esc, then the key)
	// A held ESC followed by a plain key is Esc, then the key.
	d = keyDecoder{holdEsc: true}
	d.push([]byte("\x1b"))
	if evs := d.push([]byte("x\r")); len(evs) != 3 || evs[0].code != keyEsc || evs[1].r != 'x' || evs[2].code != keyEnter {
		t.Fatalf("esc then keys: %+v", evs)
	}
	d.push([]byte("\x1b"))
	if evs := d.push([]byte("[A")); len(evs) != 1 || evs[0].code != keyUp {
		t.Fatalf("esc continued by its sequence: %+v", evs)
	}
	// A second ESC continues it; a lone non-printable byte does not.
	d.push([]byte("\x1b"))
	if evs := d.push([]byte("\x7f")); len(evs) != 2 || evs[0].code != keyEsc || evs[1].code != keyBackspace {
		t.Fatalf("esc then backspace: %+v", evs)
	}
	// Two pastes, the second begun in the read that ends the first, are
	// counted apart, so each gets its own wait.
	d = keyDecoder{holdEsc: true}
	if evs := d.push([]byte("\x1b[200~a\x1b[201~\x1b[200~b")); len(evs) != 1 || d.pastes != 2 || !d.pasting() {
		t.Fatalf("two pastes: %+v pastes %d", evs, d.pastes)
	}
	d = keyDecoder{holdEsc: true}
	// One printable byte after it is Alt and that key, split across reads.
	d.push([]byte("\x1b"))
	if evs := d.push([]byte("x")); len(evs) != 1 || evs[0].r != 'x' || evs[0].mods != modAlt {
		t.Fatalf("alt-x split across reads: %+v", evs)
	}

	// Value: protects=after a paste is given up, keys are dropped only until a quiet expire and then work, Ctrl-C included; fails_when=held ignores discarding, so the first key after a give-up is lost; why_new=the give-up path was tested with expire called by hand; seam=none; no_claim=runInbox's timer lengths
	// After a give-up the decoder is held, so the caller's quiet timer
	// runs; until it fires keys are the paste's rest, then they work.
	d = keyDecoder{holdEsc: true}
	d.push([]byte("\x1b[200~half a paste"))
	d.expire()
	d.expire()
	if !d.held() || !d.pasting() {
		t.Fatalf("given up: held %v pasting %v", d.held(), d.pasting())
	}
	if evs := d.push([]byte("\x03")); len(evs) != 0 {
		t.Fatalf("ctrl-c inside the rest of a paste: %+v", evs)
	}
	d.expire()
	if evs := d.push([]byte("\x03")); d.held() || len(evs) != 1 || evs[0].r != 'c' || evs[0].mods != modCtrl {
		t.Fatalf("ctrl-c after the quiet expire: %+v", evs)
	}

	// Value: protects=an ESC that ends a read after other keys is held too, so a paste typed right after a key stays a paste; fails_when=only a read that is ESC alone is held; why_new=the held ESC was tested alone in its read; seam=none; no_claim=reads split elsewhere in the marker
	// An ESC at the end of a read with keys before it is held as well.
	d = keyDecoder{holdEsc: true}
	if evs := d.push([]byte("a\x1b")); len(evs) != 1 || evs[0].r != 'a' || !d.held() {
		t.Fatalf("key then esc: %+v held %v", evs, d.held())
	}
	if evs := d.push([]byte("[200~x\x1b[201~")); len(evs) != 1 || !evs[0].isPaste || evs[0].paste != "x" {
		t.Fatalf("paste after a key: %+v", evs)
	}
	d.push([]byte("a\x1b"))
	if evs := d.expire(); len(evs) != 1 || evs[0].code != keyEsc {
		t.Fatalf("expired esc after a key: %+v", evs)
	}
}
