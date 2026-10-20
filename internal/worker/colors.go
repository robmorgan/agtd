package worker

import (
	"bytes"
	"fmt"

	"github.com/robmorgan/agtd/internal/protocol"
)

// Default colour queries.
//
// Programs such as Codex ask the terminal for its default foreground and
// background at startup (OSC 10 and OSC 11 with "?") and style themselves
// by the answer: Codex draws its composer's background only when both
// arrive, and waits 250 ms at most. libghostty has no default colours to
// answer with, and an attached terminal's answer takes a round trip, which
// over a long link is more than that. So the worker answers itself, from
// the colours the creating or attaching terminal reported (Args.Colors,
// AttachSession.Colors), whether or not a terminal is attached, and removes
// the query from the output: an attached terminal must not answer a second
// time, late, with what the program would take for typed text. With no
// colours known the queries pass through as before.

// colorQueryPatterns are the exact query forms, with either terminator.
var colorQueryPatterns = [...]string{"\x1b]10;?\x07", "\x1b]10;?\x1b\\", "\x1b]11;?\x07", "\x1b]11;?\x1b\\"}

// colorQueries finds the colour queries in PTY output across chunk
// boundaries.
type colorQueries struct {
	// held is the end of the last chunk: the start of a query the next
	// chunk may complete, since PTY reads split sequences routinely.
	held []byte
}

// scan removes the colour queries from data, returning what to publish and
// the replies to queue, in order. With no colours it changes nothing. A
// chunk ending in part of a query keeps that part back for the next chunk
// (or flush); the rest of the chunk is published now.
func (q *colorQueries) scan(data []byte, colors *protocol.TerminalColors) (out []byte, replies [][]byte) {
	if len(q.held) > 0 {
		data = append(q.held, data...)
		q.held = nil
	}
	if colors == nil {
		return data, nil
	}
	start := 0 // data[start:] is not yet in out
	for i := 0; i < len(data); {
		j := bytes.IndexByte(data[i:], 0x1b)
		if j < 0 {
			break
		}
		i += j
		n, slot, partial := matchColorQuery(data[i:])
		if partial {
			q.held = append([]byte(nil), data[i:]...)
			if start == 0 {
				return data[:i], replies
			}
			return append(out, data[start:i]...), replies
		}
		if n == 0 {
			i++
			continue
		}
		out = append(out, data[start:i]...)
		replies = append(replies, colorReply(slot, colors, data[i+n-1] == 0x07))
		i += n
		start = i
	}
	if start == 0 {
		return data, nil
	}
	return append(out, data[start:]...), replies
}

// flush returns the part of a query held back, giving up on its end.
func (q *colorQueries) flush() []byte {
	held := q.held
	q.held = nil
	return held
}

// matchColorQuery reports a colour query at the start of b: its length and
// slot (10 or 11), or partial when b ends before the query could.
func matchColorQuery(b []byte) (n, slot int, partial bool) {
	for _, pattern := range colorQueryPatterns {
		if bytes.HasPrefix(b, []byte(pattern)) {
			return len(pattern), int(pattern[3]-'0') + 10, false
		}
	}
	for _, pattern := range colorQueryPatterns {
		if len(b) < len(pattern) && bytes.HasPrefix([]byte(pattern), b) {
			return 0, 0, true
		}
	}
	return 0, 0, false
}

// colorReply is the terminal's answer to a query for slot, ended as the
// query was (BEL or ST), in the 16-bit form xterm introduced.
func colorReply(slot int, colors *protocol.TerminalColors, bel bool) []byte {
	c := colors.Foreground
	if slot == 11 {
		c = colors.Background
	}
	end := "\x1b\\"
	if bel {
		end = "\x07"
	}
	return []byte(fmt.Sprintf("\x1b]%d;rgb:%02x%02x/%02x%02x/%02x%02x%s", slot, c.R, c.R, c.G, c.G, c.B, c.B, end))
}
