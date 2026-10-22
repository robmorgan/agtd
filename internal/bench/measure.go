package bench

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"agtd.sh/internal/db"
	"agtd.sh/internal/protocol"
	"agtd.sh/internal/session"
)

func isFlood(id string) bool { return strings.HasPrefix(id, "flood-") }

// measureOutput runs the output-heavy sessions with no client attached and
// measures the PTY output their workers take in: the cost of feeding
// libghostty, with nothing to fan out to.
func (b *bench) measureOutput(ctx context.Context) error {
	n := b.opts.OutputHeavy
	if n == 0 {
		return nil
	}
	if err := b.parallel(ctx, n, func(i int) error {
		_, err := b.create(floodName(i), "flood")
		return err
	}); err != nil {
		return err
	}
	if err := sleep(ctx, settle); err != nil {
		return err
	}
	p, err := b.sessionPIDs(isFlood)
	if err != nil {
		return err
	}
	sample := func() ([]*protocol.SessionStats, error) {
		out := make([]*protocol.SessionStats, n)
		err := b.parallel(ctx, n, func(i int) error {
			var err error
			out[i], err = b.sessionStats(floodName(i), false)
			return err
		})
		return out, err
	}
	start := time.Now()
	w0, a0 := cpuOf(p.workers), cpuOf(p.agents)
	before, err := sample()
	if err != nil {
		return err
	}
	if err := sleep(ctx, b.opts.Duration); err != nil {
		return err
	}
	after, err := sample()
	if err != nil {
		return err
	}
	w1, a1 := cpuOf(p.workers), cpuOf(p.agents)
	wall := time.Since(start)

	var total, chunks uint64
	rates := make([]float64, n)
	workers := make([]processStats, n)
	for i := range n {
		d := after[i].OutputBytes - before[i].OutputBytes
		total += d
		chunks += after[i].OutputChunks - before[i].OutputChunks
		rates[i] = float64(d) / wall.Seconds()
		workers[i] = after[i].Worker
	}
	o := &OutputReport{
		Sessions:              n,
		IntervalSec:           wall.Seconds(),
		BytesPerSec:           float64(total) / wall.Seconds(),
		PerSessionBytesPerSec: summarize(rates),
		WorkersCPUPercent:     percent(w1-w0, wall),
		AgentsCPUPercent:      percent(a1-a0, wall),
		Workers:               distOf(workers),
	}
	if total > 0 {
		o.CPUMsPerMiB = millis(w1-w0) / (float64(total) / (1 << 20))
		o.ChunkBytes = float64(total) / float64(chunks)
	}
	b.report.Output = o
	// Fan-out is measured on the first of them alone, so stop the rest.
	return b.parallel(ctx, n-1, func(i int) error {
		_, err := b.request(&protocol.Request{KillSession: &protocol.KillSession{SessionID: floodName(i + 1), Remove: true}})
		return err
	})
}

// echoInterval is how often the fan-out phase types a marker into the
// session. The PTY echoes it into the output every client receives.
const echoInterval = 100 * time.Millisecond

// fanOutClient is one client attached during the fan-out phase. Its reader
// goroutine is the only writer of everything but received and frames,
// which the phase reads while it runs.
type fanOutClient struct {
	a *attachment
	// framed is whether the client receives screen frames rather than
	// the raw output.
	framed bool
	// received is the PTY output a raw client received.
	received atomic.Uint64
	// frames counts the frames a frame client received.
	frames atomic.Uint64
	// seen maps an echo marker to when this client first received it.
	seen  map[int]time.Time
	carry []byte
}

// bytes is what the client has received: the PTY output for a raw
// client, the bytes on the wire (frames and their framing) for a frame
// client, whose frames carry rows rather than output.
func (c *fanOutClient) bytes() uint64 {
	if c.framed {
		return c.a.wire.Load()
	}
	return c.received.Load()
}

// markerPrefix and markerSuffix frame an echo marker's sequence number.
const (
	markerPrefix = "zq"
	markerSuffix = "qz"
	markerDigits = 6
	markerLen    = len(markerPrefix) + markerDigits + len(markerSuffix)
)

func marker(seq int) string {
	return fmt.Sprintf("%s%0*d%s", markerPrefix, markerDigits, seq, markerSuffix)
}

// scan records the markers in data, including one split across reads.
func (c *fanOutClient) scan(data []byte, now time.Time) {
	buf := append(c.carry, data...)
	c.markers(buf, now)
	keep := min(len(buf), markerLen-1)
	c.carry = append(c.carry[:0], buf[len(buf)-keep:]...)
}

// markers records the markers in buf, which is scanned whole (a row).
func (c *fanOutClient) markers(buf []byte, now time.Time) {
	for i := 0; ; {
		j := bytes.Index(buf[i:], []byte(markerPrefix))
		if j < 0 || i+j+markerLen > len(buf) {
			break
		}
		at := i + j
		if string(buf[at+markerLen-len(markerSuffix):at+markerLen]) == markerSuffix {
			if seq, err := strconv.Atoi(string(buf[at+len(markerPrefix) : at+len(markerPrefix)+markerDigits])); err == nil {
				if _, ok := c.seen[seq]; !ok {
					c.seen[seq] = now
				}
			}
		}
		i = at + 1
	}
}

// read takes the client's responses until its stream ends. A frame client
// acknowledges each frame at once, as agt attach does, and looks for the
// markers in the frame's rows and in the rows that scrolled off between
// frames.
func (c *fanOutClient) read() {
	for {
		resp, err := protocol.ReadResponse(c.a.r)
		if err != nil || resp == nil {
			return
		}
		now := time.Now()
		switch {
		case resp.PtyOutput != nil:
			c.received.Add(uint64(len(resp.PtyOutput.Data)))
			c.scan(resp.PtyOutput.Data, now)
		case resp.AttachFrame != nil:
			f := resp.AttachFrame
			c.frames.Add(1)
			for _, row := range f.History {
				c.markers(row, now)
			}
			for _, row := range f.Rows {
				c.markers(row.VT, now)
			}
			if c.a.write(&protocol.Request{AttachFrameAck: &protocol.FrameAck{Seq: f.Seq}}) != nil {
				return
			}
		}
	}
}

func (b *bench) measureFanOutRaw(ctx context.Context) error {
	if b.opts.AttachMode == "frames" {
		return nil
	}
	return b.measureFanOut(ctx, false)
}

func (b *bench) measureFanOutFrames(ctx context.Context) error {
	if b.opts.AttachMode == "raw" {
		return nil
	}
	return b.measureFanOut(ctx, true)
}

// measureFanOut attaches clients to one output-heavy session, for the raw
// output or for screen frames, and measures what each receives, what the
// worker drops for them, how long typed input takes to come back to all
// of them, and what the worker's CPU and memory come to with them attached.
func (b *bench) measureFanOut(ctx context.Context, frames bool) error {
	k := b.opts.Attach
	if k == 0 {
		return nil
	}
	id := floodName(0)
	p, err := b.sessionPIDs(func(s string) bool { return s == id })
	if err != nil {
		return err
	}
	// The worker with no clients, once the previous phase's have gone.
	if err := sleep(ctx, settle); err != nil {
		return err
	}
	alone, err := b.sessionStats(id, false)
	if err != nil {
		return err
	}
	clients := make([]*fanOutClient, 0, k)
	var readers sync.WaitGroup
	defer func() {
		for _, c := range clients {
			c.a.s.Close()
		}
		readers.Wait()
	}()
	for range k {
		a, _, err := b.attach(id, frames)
		if err != nil {
			return err
		}
		c := &fanOutClient{a: a, framed: frames, seen: map[int]time.Time{}}
		clients = append(clients, c)
		readers.Add(1)
		go func() {
			defer readers.Done()
			c.read()
		}()
	}
	if err := sleep(ctx, settle); err != nil {
		return err
	}

	before, err := b.sessionStats(id, false)
	if err != nil {
		return err
	}
	received0 := make([]uint64, k)
	frames0 := make([]uint64, k)
	for i, c := range clients {
		received0[i] = c.bytes()
		frames0[i] = c.frames.Load()
	}
	daemon := []int{b.daemon.Process.Pid}
	start := time.Now()
	wc0, dc0 := cpuOf(p.workers), cpuOf(daemon)

	// Type markers on the first client's stream until the interval ends.
	var sentMu sync.Mutex
	sent := map[int]time.Time{}
	stopSending := make(chan struct{})
	senderDone := make(chan struct{})
	go func() {
		defer close(senderDone)
		ticker := time.NewTicker(echoInterval)
		defer ticker.Stop()
		for seq := 0; ; seq++ {
			select {
			case <-stopSending:
				return
			case <-ticker.C:
			}
			sentMu.Lock()
			sent[seq] = time.Now()
			sentMu.Unlock()
			in := &protocol.Request{AttachInput: &protocol.Bytes{Data: []byte(marker(seq) + "\n")}}
			if clients[0].a.write(in) != nil {
				return
			}
		}
	}()
	err = sleep(ctx, b.opts.Duration)
	close(stopSending)
	<-senderDone
	if err != nil {
		return err
	}
	after, err := b.sessionStats(id, false)
	if err != nil {
		return err
	}
	wall := time.Since(start)
	wc1, dc1 := cpuOf(p.workers), cpuOf(daemon)
	received := make([]uint64, k)
	frameRates := make([]float64, k)
	for i, c := range clients {
		received[i] = c.bytes() - received0[i]
		frameRates[i] = float64(c.frames.Load()-frames0[i]) / wall.Seconds()
	}
	// Let the last markers arrive, then stop the readers so their maps can
	// be read.
	if err := sleep(ctx, 500*time.Millisecond); err != nil {
		return err
	}
	for _, c := range clients {
		c.a.s.Close()
	}
	readers.Wait()

	produced := after.OutputBytes - before.OutputBytes
	rates := make([]float64, k)
	delivered := make([]float64, k)
	for i, r := range received {
		rates[i] = float64(r) / wall.Seconds()
		if produced > 0 {
			// A raw client received a part of the output; a frame
			// client received so many bytes per byte of it.
			delivered[i] = float64(r) / float64(produced)
			if !frames {
				delivered[i] = min(1, delivered[i])
			}
		}
	}
	var echoes []float64
	lost := 0
	for seq, at := range sent {
		for _, c := range clients {
			if got, ok := c.seen[seq]; ok {
				echoes = append(echoes, millis(got.Sub(at)))
			} else {
				lost++
			}
		}
	}
	rep := &FanOutReport{
		Mode:              "raw",
		Clients:           k,
		IntervalSec:       wall.Seconds(),
		PTYBytesPerSec:    float64(produced) / wall.Seconds(),
		ClientBytesPerSec: summarize(rates),
		Delivered:         summarize(delivered),
		DroppedChunks:     after.DroppedOutputChunks - before.DroppedOutputChunks,
		ChunkBytes:        float64(produced) / float64(max(1, after.OutputChunks-before.OutputChunks)),
		EchoMs:            summarize(echoes),
		EchoesLost:        lost,
		WorkerCPU:         percent(wc1-wc0, wall),
		DaemonCPU:         percent(dc1-dc0, wall),
		WorkerAlone:       sampleOf(alone.Worker),
		Worker:            sampleOf(after.Worker),
	}
	if frames {
		rep.Mode = "frames"
		rep.FramesPerSec = summarize(frameRates)
		b.report.FanOutFrames = rep
	} else {
		b.report.FanOut = rep
	}
	return nil
}

// Repetitions of the database measurements.
const (
	listRepeats = 10
	getRepeats  = 50
)

func timeIt(n int, fn func() error) (Dist, error) {
	samples := make([]float64, 0, n)
	for range n {
		start := time.Now()
		if err := fn(); err != nil {
			return Dist{}, err
		}
		samples = append(samples, millis(time.Since(start)))
	}
	return summarize(samples), nil
}

// measureDatabase times session lookups through the daemon and against
// state.db directly, and inserts into a copy-sized scratch database.
func (b *bench) measureDatabase(ctx context.Context) error {
	store, err := db.Open(b.paths.Database)
	if err != nil {
		return err
	}
	recs, err := store.ListSessions()
	if err != nil {
		return err
	}
	r := DatabaseReport{Rows: len(recs)}
	if info, err := os.Stat(b.paths.Database); err == nil {
		r.FileBytes = uint64(info.Size())
	}
	id := ""
	if len(recs) > 0 {
		id = recs[len(recs)/2].SessionID
	}
	if r.ListSessionsMs, err = timeIt(listRepeats, func() error {
		_, err := b.request(&protocol.Request{ListSessions: protocol.Empty})
		return err
	}); err != nil {
		return err
	}
	if r.ListSessionsDBMs, err = timeIt(listRepeats, func() error {
		_, err := store.ListSessions()
		return err
	}); err != nil {
		return err
	}
	if id != "" {
		if r.GetSessionMs, err = timeIt(getRepeats, func() error {
			_, err := b.request(&protocol.Request{GetSession: &protocol.SessionRef{SessionID: id}})
			return err
		}); err != nil {
			return err
		}
		if r.GetSessionDBMs, err = timeIt(getRepeats, func() error {
			_, err := store.GetSession(id)
			return err
		}); err != nil {
			return err
		}
	}
	if err := ctx.Err(); err != nil {
		return err
	}

	// Inserts go to a scratch database with as many rows as state.db ends
	// up with, so the live one is not disturbed.
	scratch, err := db.Open(filepath.Join(b.root, "bench-insert.db"))
	if err != nil {
		return err
	}
	if r.InsertDBMs, err = func() (Dist, error) {
		i := 0
		return timeIt(max(len(recs), 1), func() error {
			i++
			_, err := scratch.InsertSession(db.NewSession{
				SessionID: fmt.Sprintf("s-%05d", i), Agent: "idle", Mode: session.ModeExecute, Cwd: b.workDir(),
			})
			return err
		})
	}(); err != nil {
		return err
	}
	b.report.Database = r
	return nil
}
