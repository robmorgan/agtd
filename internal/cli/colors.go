package cli

import (
	"io"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"agtd.sh/internal/protocol"
	"golang.org/x/sys/unix"
)

// The terminal's default colours.
//
// Programs such as Codex ask the terminal for its default foreground and
// background at startup (OSC 10 and OSC 11) and style themselves by the
// answer, but wait only briefly: over a long link the terminal's answer
// comes too late, and with nobody attached nothing answers at all. So the
// CLI asks its own terminal once, at the first `agt new` or attach, and
// sends the answer with CreateSession and AttachSession (CapTerminalColors)
// for the worker to answer the program with at once.

const (
	// colorProbe asks for both colours, then for the primary device
	// attributes, which every terminal answers: its reply says the
	// terminal has answered all it will.
	colorProbe = "\x1b]10;?\x1b\\\x1b]11;?\x1b\\\x1b[c"
	// colorProbeTimeout bounds the wait for a terminal that answers
	// nothing (a tty that is not a terminal emulator).
	colorProbeTimeout = 300 * time.Millisecond
	// colorProbeGrace is how long a terminal that has answered the device
	// attributes without the colours gets to answer them still: a
	// multiplexer answers the former itself and relays the latter.
	colorProbeGrace = 50 * time.Millisecond
)

var (
	colorsOnce   sync.Once
	colorsCached *protocol.TerminalColors
	// stdinLeftover is what the terminal sent during the probe that was
	// not a reply (keys typed early), for stdinChunks to deliver first.
	stdinLeftover []byte
)

// terminalColors asks the terminal on stdin and stdout for its default
// colours, once per process, before anything else reads stdin
// (stdinChunks calls it first). Nil without a terminal, or when it does
// not answer.
func terminalColors() *protocol.TerminalColors {
	colorsOnce.Do(func() {
		if !isTerminal(os.Stdin) || !isTerminal(os.Stdout) || os.Getenv("TERM") == "dumb" {
			return
		}
		colorsCached, stdinLeftover = probeTerminalColors(int(os.Stdin.Fd()), os.Stdout, colorProbeTimeout)
	})
	return colorsCached
}

// probeTerminalColors writes colorProbe to out and reads the terminal's
// replies from fd, in raw mode so they are neither echoed nor held for a
// newline. It returns the colours, if both arrived, and the bytes that
// were not replies.
func probeTerminalColors(fd int, out io.Writer, timeout time.Duration) (*protocol.TerminalColors, []byte) {
	orig, err := unix.IoctlGetTermios(fd, ioctlGetTermios)
	if err != nil {
		return nil, nil
	}
	raw := *orig
	raw.Lflag &^= unix.ECHO | unix.ICANON | unix.ISIG | unix.IEXTEN
	raw.Iflag &^= unix.ICRNL | unix.IXON
	raw.Cc[unix.VMIN], raw.Cc[unix.VTIME] = 1, 0
	if err := unix.IoctlSetTermios(fd, ioctlSetTermios, &raw); err != nil {
		return nil, nil
	}
	// Put back without flushing: keys typed meanwhile are kept.
	defer unix.IoctlSetTermios(fd, ioctlSetTermios, orig)
	if _, err := out.Write([]byte(colorProbe)); err != nil {
		return nil, nil
	}
	var buf []byte
	var replies colorReplies
	deadline := time.Now().Add(timeout)
	graced := false
	for {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			break
		}
		fds := []unix.PollFd{{Fd: int32(fd), Events: unix.POLLIN}}
		n, err := unix.Poll(fds, int(remaining/time.Millisecond)+1)
		if err == unix.EINTR {
			continue
		}
		if err != nil || n == 0 {
			break
		}
		chunk := make([]byte, 1024)
		m, err := unix.Read(fd, chunk)
		if m > 0 {
			buf = append(buf, chunk[:m]...)
		} else if err != unix.EINTR && err != unix.EAGAIN {
			break
		}
		replies = parseColorReplies(buf)
		if replies.done() {
			break
		}
		if replies.da1 && !graced {
			graced = true
			if grace := time.Now().Add(colorProbeGrace); grace.Before(deadline) {
				deadline = grace
			}
		}
	}
	replies = parseColorReplies(buf)
	return replies.colors(), replies.rest
}

// colorReplies is what parseColorReplies found in the terminal's bytes.
type colorReplies struct {
	fg, bg *protocol.RGB
	da1    bool
	// rest is everything that was not a reply, in order: keys typed, and
	// an unfinished sequence at the end.
	rest []byte
}

func (r colorReplies) done() bool { return r.da1 && r.fg != nil && r.bg != nil }

func (r colorReplies) colors() *protocol.TerminalColors {
	if r.fg == nil || r.bg == nil {
		return nil
	}
	return &protocol.TerminalColors{Foreground: *r.fg, Background: *r.bg}
}

// parseColorReplies picks the colour replies (OSC 10 and 11) and the
// device attributes reply (CSI ? ... c) out of buf.
func parseColorReplies(buf []byte) colorReplies {
	var r colorReplies
	for i := 0; i < len(buf); {
		if buf[i] != 0x1b {
			r.rest = append(r.rest, buf[i])
			i++
			continue
		}
		if i+1 >= len(buf) {
			r.rest = append(r.rest, buf[i:]...)
			break
		}
		switch buf[i+1] {
		case ']':
			end, term := oscEnd(buf[i+2:])
			if end < 0 {
				r.rest = append(r.rest, buf[i:]...)
				return r
			}
			slot, payload, _ := strings.Cut(string(buf[i+2:i+2+end]), ";")
			c := parseOSCColor(payload)
			switch {
			case c != nil && slot == "10":
				r.fg = c
			case c != nil && slot == "11":
				r.bg = c
			default:
				r.rest = append(r.rest, buf[i:i+2+end+term]...)
			}
			i += 2 + end + term
		case '[':
			j := i + 2
			for j < len(buf) && buf[j] >= 0x30 && buf[j] <= 0x3f {
				j++
			}
			for j < len(buf) && buf[j] >= 0x20 && buf[j] <= 0x2f {
				j++
			}
			if j >= len(buf) {
				r.rest = append(r.rest, buf[i:]...)
				return r
			}
			if buf[j] == 'c' && j > i+2 && buf[i+2] == '?' {
				r.da1 = true
			} else {
				r.rest = append(r.rest, buf[i:j+1]...)
			}
			i = j + 1
		default:
			r.rest = append(r.rest, buf[i])
			i++
		}
	}
	return r
}

// oscEnd finds the end of an OSC body: the index of its terminator (BEL
// or ST) and the terminator's length, or -1 when it has not arrived.
func oscEnd(b []byte) (end, term int) {
	for i, c := range b {
		switch {
		case c == 0x07:
			return i, 1
		case c == 0x1b && i+1 < len(b) && b[i+1] == '\\':
			return i, 2
		case c == 0x1b && i+1 == len(b):
			return -1, 0
		}
	}
	return -1, 0
}

// parseOSCColor reads a colour as terminals report it: "rgb:RR/GG/BB" with
// one to four hex digits per channel (xterm and most others), "rgba:" with
// an alpha channel after them, or "#RRGGBB" in the X11 forms.
func parseOSCColor(s string) *protocol.RGB {
	var parts []string
	switch {
	case strings.HasPrefix(strings.ToLower(s), "rgb:"):
		parts = strings.Split(s[4:], "/")
	case strings.HasPrefix(strings.ToLower(s), "rgba:"):
		parts = strings.Split(s[5:], "/")
		if len(parts) != 4 {
			return nil
		}
		parts = parts[:3]
	case strings.HasPrefix(s, "#") && len(s)%3 == 1 && len(s) >= 4 && len(s) <= 13:
		n := (len(s) - 1) / 3
		parts = []string{s[1 : 1+n], s[1+n : 1+2*n], s[1+2*n:]}
	default:
		return nil
	}
	if len(parts) != 3 {
		return nil
	}
	var c [3]uint8
	for i, p := range parts {
		v, ok := scaleHex(p)
		if !ok {
			return nil
		}
		c[i] = v
	}
	return &protocol.RGB{R: c[0], G: c[1], B: c[2]}
}

// scaleHex scales one to four hex digits to 8 bits.
func scaleHex(s string) (uint8, bool) {
	if len(s) < 1 || len(s) > 4 {
		return 0, false
	}
	v, err := strconv.ParseUint(s, 16, 16)
	if err != nil {
		return 0, false
	}
	max := uint64(1)<<(4*uint(len(s))) - 1
	return uint8((v*255 + max/2) / max), true
}
