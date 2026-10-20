package cli

import (
	"bytes"
	"testing"
	"time"

	"github.com/creack/pty"
	"agtd.sh/internal/protocol"
)

func TestParseColorReplies(t *testing.T) {
	fg, bg := protocol.RGB{R: 0xdd, G: 0xdd, B: 0xdd}, protocol.RGB{R: 0x1e, G: 0x1e, B: 0x2e}
	r := parseColorReplies([]byte("\x1b]10;rgb:dddd/dddd/dddd\x1b\\\x1b]11;rgb:1e1e/1e1e/2e2e\x1b\\\x1b[?62;22c"))
	if !r.done() || *r.fg != fg || *r.bg != bg || len(r.rest) != 0 {
		t.Fatalf("full replies: %+v", r)
	}
	if c := r.colors(); c == nil || c.Foreground != fg || c.Background != bg {
		t.Fatalf("colors() = %+v", c)
	}
	// BEL-terminated, short and X11 forms, with keys around the replies.
	r = parseColorReplies([]byte("k\x1b]10;rgb:f/e/d\x07\x1b[A\x1b]11;#1e1e2e\x07\x1b[?1;2cq"))
	if !r.done() || *r.fg != (protocol.RGB{R: 0xff, G: 0xee, B: 0xdd}) || *r.bg != bg || string(r.rest) != "k\x1b[Aq" {
		t.Fatalf("short forms: %+v rest %q", r, r.rest)
	}
	// rgba scales each channel and drops the alpha.
	r = parseColorReplies([]byte("\x1b]10;rgba:ffff/8000/0000/ffff\x1b\\"))
	if r.done() || r.fg == nil || *r.fg != (protocol.RGB{R: 0xff, G: 0x80, B: 0}) || r.bg != nil || r.colors() != nil {
		t.Fatalf("rgba: %+v", r)
	}
	// Device attributes alone: the terminal reports no colours.
	if r = parseColorReplies([]byte("\x1b[?64;1;2c")); !r.da1 || r.colors() != nil || len(r.rest) != 0 {
		t.Fatalf("DA1 alone: %+v", r)
	}
	// An unfinished reply at the end stays in rest.
	for _, s := range []string{"\x1b]11;rgb:1e", "\x1b", "\x1b[?62;2", "\x1b]11;rgb:1e1e/1e1e/2e2e\x1b"} {
		if r = parseColorReplies([]byte(s)); r.done() || r.bg != nil || string(r.rest) != s {
			t.Fatalf("unfinished %q: %+v rest %q", s, r, r.rest)
		}
	}
	// Other slots, and payloads that are not colours, are kept as keys.
	s := "\x1b]12;rgb:0/0/0\x1b\\\x1b]10;bogus\x07\x1bx"
	if r = parseColorReplies([]byte(s)); r.fg != nil || string(r.rest) != s {
		t.Fatalf("other sequences: %+v rest %q", r, r.rest)
	}
	for _, bad := range []string{"rgb:fffff/0/0", "rgb:f/f", "#12345", "rgba:f/f/f", "rgb:g/0/0", ""} {
		if c := parseOSCColor(bad); c != nil {
			t.Fatalf("parseOSCColor(%q) = %+v", bad, c)
		}
	}
	if c := parseOSCColor("RGB:1/22/333"); c == nil || *c != (protocol.RGB{R: 0x11, G: 0x22, B: 0x33}) {
		t.Fatalf("mixed widths: %+v", c)
	}
}

// The probe reads the terminal's replies through a pty, keeps keys typed
// meanwhile, and gives up on a terminal that does not answer.
func TestProbeTerminalColors(t *testing.T) {
	ptmx, tty, err := pty.Open()
	if err != nil {
		t.Fatal(err)
	}
	defer ptmx.Close()
	defer tty.Close()
	type result struct {
		colors *protocol.TerminalColors
		rest   []byte
	}
	probe := func(timeout time.Duration) <-chan result {
		ch := make(chan result, 1)
		go func() {
			c, rest := probeTerminalColors(int(tty.Fd()), tty, timeout)
			ch <- result{c, rest}
		}()
		return ch
	}
	// The terminal side reads the probe before answering.
	readProbe := func() {
		t.Helper()
		var seen []byte
		buf := make([]byte, 256)
		for !bytes.Contains(seen, []byte(colorProbe)) {
			n, err := ptmx.Read(buf)
			if err != nil {
				t.Fatal(err)
			}
			seen = append(seen, buf[:n]...)
		}
	}
	await := func(ch <-chan result) result {
		t.Helper()
		select {
		case r := <-ch:
			return r
		case <-time.After(5 * time.Second):
			t.Fatal("the probe did not finish")
			return result{}
		}
	}

	ch := probe(2 * time.Second)
	readProbe()
	ptmx.Write([]byte("k\x1b]10;rgb:dddd/dddd/dddd\x1b\\\x1b]11;rgb:1e1e/1e1e/2e2e\x1b\\\x1b[?62;22c"))
	r := await(ch)
	want := &protocol.TerminalColors{Foreground: protocol.RGB{R: 0xdd, G: 0xdd, B: 0xdd}, Background: protocol.RGB{R: 0x1e, G: 0x1e, B: 0x2e}}
	if r.colors == nil || *r.colors != *want || string(r.rest) != "k" {
		t.Fatalf("probe = %+v, rest %q", r.colors, r.rest)
	}

	// No answer at all: nil, within the timeout.
	start := time.Now()
	ch = probe(100 * time.Millisecond)
	readProbe()
	if r = await(ch); r.colors != nil || len(r.rest) != 0 || time.Since(start) > time.Second {
		t.Fatalf("silent terminal: %+v, rest %q after %v", r.colors, r.rest, time.Since(start))
	}

	// Device attributes without colours end the wait early.
	start = time.Now()
	ch = probe(2 * time.Second)
	readProbe()
	ptmx.Write([]byte("\x1b[?62c"))
	if r = await(ch); r.colors != nil || time.Since(start) > time.Second {
		t.Fatalf("DA1 without colours: %+v after %v", r.colors, time.Since(start))
	}
}
