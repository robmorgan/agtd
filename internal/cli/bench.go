package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"sort"
	"strings"
	"time"

	"agtd.sh/internal/protocol"
	"github.com/spf13/cobra"
)

// agt bench echo measures typing over a session's link: a developer's
// command, hidden, for the numbers in BENCHMARKS.md ("Remote interaction").
// It attaches like agt attach does, types markers the session echoes back
// (a shell's line editing or cat's line discipline echo them at once; an
// agent redraws its box), and reports how long each took to come back, and
// with --field how long until the worker's input field showed it, which is
// what the local echo waits for. --burst times a burst of output from a
// shell session, counting the resyncs a lagging client is given; --watch
// reports the output rate of whatever the session is doing.

// benchMarker is the marker's form, the one `agtd bench sessions` types
// (internal/bench): a prefix, six digits and a suffix that no agent's
// output contains by accident.
func benchMarker(seq int) string { return fmt.Sprintf("zq%06dqz", seq) }

// benchTimeout bounds each wait for the session.
const benchTimeout = 10 * time.Second

type echoBenchOptions struct {
	count    int
	interval time.Duration
	field    bool
	burst    int
	watch    time.Duration
}

func (a *app) benchCommands() *cobra.Command {
	var opts echoBenchOptions
	echo := a.command("echo [HOST/]SESSION", "Measure keystroke echo through a session", oneArg, func(args []string) error {
		id := args[0]
		c, err := a.connect([]*string{&id}, "", nil)
		if err != nil {
			return err
		}
		return c.benchEcho(id, opts, os.Stdout)
	})
	echo.Flags().IntVar(&opts.count, "count", 20, "markers to type")
	echo.Flags().DurationVar(&opts.interval, "interval", 250*time.Millisecond, "pause between markers")
	echo.Flags().BoolVar(&opts.field, "field", false, "also time each marker's confirmation by the input field")
	echo.Flags().IntVar(&opts.burst, "burst", 0, "also time a burst of this many bytes of output (a shell session)")
	echo.Flags().DurationVar(&opts.watch, "watch", 0, "also watch the output for this long and report its rate")
	g := group("bench", "Measure a session over its link", "", "", echo)
	g.Hidden = true
	return g
}

// echoBench is one attachment used for measuring.
type echoBench struct {
	stream *attachStream
	// carry is the end of the last output, for a marker split across frames.
	carry []byte
	// Counters since reset.
	bytes, frames, resyncs int
}

var errBenchTimeout = errors.New("timed out waiting for the session")

func (c *client) benchEcho(id string, opts echoBenchOptions, out io.Writer) error {
	stream, attached, ended, err := c.connectAttach(context.Background(), id, attachIdentity{}, 0, false)
	if err != nil {
		return err
	}
	if ended != nil {
		return fmt.Errorf("session `%s` has ended", id)
	}
	defer stream.leave()
	if opts.field && !attached.HasFeature(protocol.CapAttachField) {
		return errors.New("the daemon does not report the input field (attach-field)")
	}
	where := "local"
	if c.host != nil {
		where = c.host.Name
	}
	fmt.Fprintf(out, "session %s on %s (%s)\n", id, where, attached.AttachID)
	b := &echoBench{stream: stream}
	// Let the attach's output settle.
	if err := b.drain(300 * time.Millisecond); err != nil {
		return err
	}

	var echoes, confirms []time.Duration
	var probes, perKey []int
	for seq := 1; seq <= opts.count; seq++ {
		m := benchMarker(seq)
		b.reset()
		start := time.Now()
		if err := b.send(m); err != nil {
			return err
		}
		if err := b.await(b.marker(m), benchTimeout); err != nil {
			return fmt.Errorf("marker %d never came back: %w", seq, err)
		}
		echoes = append(echoes, time.Since(start))
		if opts.field {
			n := 0
			for {
				n++
				if err := stream.send(&protocol.Request{AttachField: protocol.Empty}); err != nil {
					return err
				}
				var f *protocol.Field
				if err := b.await(func(r *protocol.Response) bool { f = r.Field; return f != nil }, benchTimeout); err != nil {
					return fmt.Errorf("the field was not answered: %w", err)
				}
				if f.Found && slices.ContainsFunc(f.Rows, func(row string) bool { return strings.Contains(row, m) }) {
					break
				}
				if n >= 50 {
					return fmt.Errorf("the input field never showed marker %d (found %v, rows %q)", seq, f.Found, f.Rows)
				}
			}
			confirms = append(confirms, time.Since(start))
			probes = append(probes, n)
		}
		perKey = append(perKey, b.bytes)
		// Erase the marker the way any editor allows.
		if err := b.send(strings.Repeat("\x7f", len(m))); err != nil {
			return err
		}
		if err := b.drain(opts.interval); err != nil {
			return err
		}
	}
	if opts.count > 0 {
		fmt.Fprintf(out, "echo        %s\n", benchDist(echoes))
		if opts.field {
			fmt.Fprintf(out, "confirmed   %s  (field probes per key: p50 %d, max %d)\n", benchDist(confirms), benchPercentile(probes, 50), slices.Max(probes))
		}
		fmt.Fprintf(out, "output/key  p50 %d B, max %d B (a marker is %d bytes)\n", benchPercentile(perKey, 50), slices.Max(perKey), len(benchMarker(1)))
	}

	if opts.burst > 0 {
		b.reset()
		// The command's own echo must not contain the end marker.
		cmd := fmt.Sprintf("head -c %d /dev/zero | tr '\\0' x | fold -w 100; printf 'BENCH%%s\\n' END\r", opts.burst)
		start := time.Now()
		if err := b.send(cmd); err != nil {
			return err
		}
		if err := b.await(b.marker("BENCHEND"), 10*benchTimeout); err != nil {
			return fmt.Errorf("the burst never ended: %w", err)
		}
		took := time.Since(start)
		fmt.Fprintf(out, "burst       %d B in %s: %d B received in %d frames, %d resyncs, %.0f KB/s\n",
			opts.burst, took.Round(time.Millisecond), b.bytes, b.frames, b.resyncs, float64(b.bytes)/1024/took.Seconds())
		if err := b.drain(opts.interval); err != nil {
			return err
		}
	}

	if opts.watch > 0 {
		b.reset()
		start := time.Now()
		if err := b.drain(opts.watch); err != nil {
			return err
		}
		took := time.Since(start)
		fmt.Fprintf(out, "watch       %s: %d B in %d frames, %d resyncs, %.1f KB/s\n",
			took.Round(time.Millisecond), b.bytes, b.frames, b.resyncs, float64(b.bytes)/1024/took.Seconds())
	}
	return nil
}

func (b *echoBench) send(text string) error {
	return b.stream.send(&protocol.Request{AttachInput: &protocol.Bytes{Data: []byte(text)}})
}

func (b *echoBench) reset() { b.bytes, b.frames, b.resyncs = 0, 0, 0 }

// marker is a predicate for text arriving in the output, across frames.
func (b *echoBench) marker(text string) func(*protocol.Response) bool {
	want := []byte(text)
	return func(r *protocol.Response) bool {
		var data []byte
		switch {
		case r.PtyOutput != nil:
			data = r.PtyOutput.Data
		case r.AttachResync != nil:
			data = r.AttachResync.Data
		default:
			return false
		}
		buf := append(b.carry, data...)
		found := bytes.Contains(buf, want)
		keep := min(len(buf), len(want)-1)
		b.carry = append(b.carry[:0], buf[len(buf)-keep:]...)
		return found
	}
}

// drain reads frames for d, counting them.
func (b *echoBench) drain(d time.Duration) error {
	err := b.await(func(*protocol.Response) bool { return false }, d)
	if errors.Is(err, errBenchTimeout) {
		return nil
	}
	return err
}

// await reads frames until done says so or timeout passes, counting the
// output and resyncs seen. The session ending, or the stream, is an error.
func (b *echoBench) await(done func(*protocol.Response) bool, timeout time.Duration) error {
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	for {
		select {
		case <-deadline.C:
			return errBenchTimeout
		case f := <-b.stream.frames:
			switch {
			case f.err != nil:
				return f.err
			case f.resp == nil:
				return errAttachDropped
			case f.resp.Error != nil:
				return errors.New(f.resp.Error.Message)
			case f.resp.SessionEnded != nil:
				return errors.New(formatSessionEnd(f.resp.SessionEnded))
			case f.resp.EndOfStream != nil, f.resp.SessionRestarting != nil:
				return errors.New("the attachment ended")
			case f.resp.PtyOutput != nil:
				b.bytes += len(f.resp.PtyOutput.Data)
				b.frames++
			case f.resp.AttachResync != nil:
				b.bytes += len(f.resp.AttachResync.Data)
				b.frames++
				b.resyncs++
			}
			if done(f.resp) {
				return nil
			}
		}
	}
}

// benchDist summarises durations as "n=N min p50 p90 max".
func benchDist(samples []time.Duration) string {
	if len(samples) == 0 {
		return "n=0"
	}
	s := slices.Clone(samples)
	slices.Sort(s)
	at := func(p int) time.Duration { return s[min(len(s)-1, (len(s)*p+99)/100)] }
	return fmt.Sprintf("n=%d  min %s  p50 %s  p90 %s  max %s", len(s),
		s[0].Round(100*time.Microsecond), at(50).Round(100*time.Microsecond), at(90).Round(100*time.Microsecond), s[len(s)-1].Round(100*time.Microsecond))
}

func benchPercentile(samples []int, p int) int {
	if len(samples) == 0 {
		return 0
	}
	s := slices.Clone(samples)
	sort.Ints(s)
	return s[min(len(s)-1, (len(s)*p+99)/100)]
}
