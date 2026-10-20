package worker

import (
	"strings"
	"testing"

	"agtd.sh/internal/protocol"
)

var testColors = &protocol.TerminalColors{Foreground: protocol.RGB{R: 0xdd, G: 0xdd, B: 0xdd}, Background: protocol.RGB{R: 0x1e, G: 0x1e, B: 0x2e}}

func TestColorQueryScan(t *testing.T) {
	var q colorQueries
	check := func(data string, wantOut string, wantReplies ...string) {
		t.Helper()
		out, replies := q.scan([]byte(data), testColors)
		if string(out) != wantOut || len(replies) != len(wantReplies) {
			t.Fatalf("scan(%q) = %q, %q; want %q, %q", data, out, replies, wantOut, wantReplies)
		}
		for i, r := range replies {
			if string(r) != wantReplies[i] {
				t.Fatalf("scan(%q) reply %d = %q, want %q", data, i, r, wantReplies[i])
			}
		}
	}
	// Without colours nothing changes, and the chunk is passed as it is.
	in := []byte("a\x1b]11;?\x1b\\b")
	if out, replies := q.scan(in, nil); &out[0] != &in[0] || replies != nil {
		t.Fatalf("scan without colours = %q, %q", out, replies)
	}
	// Both slots, either terminator, and the text around them kept in
	// order; the reply ends as the query did.
	check("x\x1b]10;?\x07y\x1b]11;?\x1b\\z\x1b[31m", "xyz\x1b[31m", "\x1b]10;rgb:dddd/dddd/dddd\x07", "\x1b]11;rgb:1e1e/1e1e/2e2e\x1b\\")
	// A chunk without a query is passed as it is.
	in = []byte("plain \x1b[2J text")
	if out, replies := q.scan(in, testColors); &out[0] != &in[0] || replies != nil {
		t.Fatalf("scan of plain output = %q, %q", out, replies)
	}
	// Not a colour query: setting a colour, another slot, text after the
	// "?", and the two-slot form.
	for _, s := range []string{"\x1b]11;#000000\x1b\\", "\x1b]12;?\x1b\\", "\x1b]11;?x\x1b\\", "\x1b]10;?;?\x1b\\"} {
		check(s, s)
	}
	// A query split across chunks: its start is held, the rest of the
	// chunk goes out, and the next chunk completes it.
	check("ab\x1b]1", "ab")
	if string(q.held) != "\x1b]1" {
		t.Fatalf("held %q", q.held)
	}
	check("1;?\x1b\\cd", "cd", "\x1b]11;rgb:1e1e/1e1e/2e2e\x1b\\")
	// A lone ESC at the end is held, and flushed when asked.
	check("q\x1b", "q")
	if tail := q.flush(); string(tail) != "\x1b" || q.flush() != nil {
		t.Fatalf("flush = %q", tail)
	}
	// A held start that does not become a query is published whole.
	check("\x1b]10", "")
	check(";0;rgb:1/2/3\x07", "\x1b]10;0;rgb:1/2/3\x07")
	// The held start of a query the next chunk completes, then more.
	check("\x1b]11;?\x1b", "")
	check("\\\x1b]10;?\x07!", "!", "\x1b]11;rgb:1e1e/1e1e/2e2e\x1b\\", "\x1b]10;rgb:dddd/dddd/dddd\x07")
}

// The worker answers the program's colour queries from the colours the
// creating or attaching terminal reported, attached or not, and an attached
// terminal never sees the query; without colours the query passes through.
func TestColorQueriesAnswered(t *testing.T) {
	// The agent asks for the background and prints the reply, ESCs removed.
	const querier = `stty -echo; read go; stty raw -echo min 0 time 10; printf 'ask\033]11;?\033\\'; r=$(dd bs=1 count=25 2>/dev/null | tr -d '\033'); stty sane; echo "reply:$r"; read l`
	const reply = "reply:]11;rgb:1e1e/1e1e/2e2e\\"

	created := startWorkerArgs(t, querier, testColors)
	created.sendInput("go\n")
	created.eventually("colour reply", func() bool { return strings.Contains(created.history(), reply) })
	created.sendInput("\n")
	created.waitExit()

	attached := startWorkerWith(t, querier)
	c, _ := attached.attachWith(protocol.AttachSession{Features: []string{protocol.CapTerminalColors}, Colors: testColors})
	c.input("go\n")
	c.expectOutput(reply)
	if out := c.output.String(); strings.Contains(out, "\x1b]11;?") {
		t.Fatalf("the query reached the attached terminal: %q", out)
	}
	c.input("\n")
	c.expectEnd()
	attached.waitExit()

	unknown := startWorkerWith(t, querier)
	c = unknown.attach(defaultGeometry)
	c.input("go\n")
	c.expectOutput("\x1b]11;?") // the query reaches the attached terminal
	c.expectOutput("reply:")
	c.expectOutput("\n")
	if out := c.output.String(); strings.Contains(out, "rgb:") {
		t.Fatalf("worker answered a colour query without colours: %q", out)
	}
	c.input("\n")
	c.expectEnd()
	unknown.waitExit()
}
