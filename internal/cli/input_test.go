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
		// The picker and the overlay never see mouse reports, focus
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
