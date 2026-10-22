package bench

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"sync"
	"sync/atomic"
	"time"

	"agtd.sh/internal/protocol"
	"agtd.sh/internal/session"
	"agtd.sh/internal/transport"
)

// requestTimeout bounds one request to the bench's daemon. Creating a
// session waits for its worker, which takes longer under load.
const requestTimeout = 60 * time.Second

// request sends one request on a stream of its own, as a script would,
// and returns the response. An Error response is returned as an error.
func (b *bench) request(req *protocol.Request) (*protocol.Response, error) {
	s, err := transport.DialUnix(b.paths.Socket, requestTimeout)
	if err != nil {
		return nil, err
	}
	defer s.Close()
	s.SetDeadline(time.Now().Add(requestTimeout))
	if err := protocol.WriteRequest(s, req); err != nil {
		return nil, err
	}
	resp, err := protocol.ReadResponse(bufio.NewReader(s))
	switch {
	case err != nil:
		return nil, err
	case resp == nil:
		return nil, errors.New("agtd closed the connection")
	case resp.Error != nil:
		return nil, errors.New(resp.Error.Message)
	}
	return resp, nil
}

func (b *bench) sessionStats(id string, snapshot bool) (*protocol.SessionStats, error) {
	resp, err := b.request(&protocol.Request{GetSessionStats: &protocol.GetSessionStats{SessionID: id, Snapshot: snapshot}})
	if err != nil {
		return nil, fmt.Errorf("stats for %s: %w", id, err)
	}
	if resp.SessionStats == nil {
		return nil, fmt.Errorf("stats for %s: unexpected response", id)
	}
	return resp.SessionStats, nil
}

func (b *bench) daemonStats() (protocol.ProcessStats, error) {
	resp, err := b.request(&protocol.Request{GetDaemonStats: protocol.Empty})
	if err != nil {
		return protocol.ProcessStats{}, err
	}
	if resp.DaemonStats == nil {
		return protocol.ProcessStats{}, errors.New("daemon stats: unexpected response")
	}
	return resp.DaemonStats.Daemon, nil
}

// attachment is one attached client.
type attachment struct {
	s        transport.Stream
	r        *bufio.Reader
	snapshot []byte
	// wire counts the bytes read from s, framing included.
	wire atomic.Uint64
	// wmu serialises writers: the fan-out phase types on one stream
	// while its reader acknowledges frames.
	wmu sync.Mutex
}

// write sends one request on the attachment's stream.
func (a *attachment) write(req *protocol.Request) error {
	a.wmu.Lock()
	defer a.wmu.Unlock()
	return protocol.WriteRequest(a.s, req)
}

// countingReader adds what it reads to n.
type countingReader struct {
	r io.Reader
	n *atomic.Uint64
}

func (c countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n.Add(uint64(n))
	return n, err
}

// benchGeometry is the size bench clients attach with: the worker's
// default, so attaching does not resize (and repaint) the session.
var benchGeometry = protocol.Geometry{Cols: 160, Rows: 48}

// attach attaches to a session, for the raw output or for screen frames,
// and returns once the snapshot has arrived, with the time that took: the
// attach latency a client sees, through the daemon's proxy.
func (b *bench) attach(id string, frames bool) (*attachment, time.Duration, error) {
	start := time.Now()
	s, err := transport.DialUnix(b.paths.Socket, requestTimeout)
	if err != nil {
		return nil, 0, err
	}
	s.SetDeadline(time.Now().Add(requestTimeout))
	req := &protocol.AttachSession{SessionID: id, Kind: session.AttachmentAttach, Geometry: benchGeometry}
	if frames {
		req.Features = []string{protocol.CapAttachFrames}
	}
	if err := protocol.WriteRequest(s, &protocol.Request{AttachSession: req}); err != nil {
		s.Close()
		return nil, 0, err
	}
	a := &attachment{s: s}
	a.r = bufio.NewReaderSize(countingReader{r: s, n: &a.wire}, 64<<10)
	resp, err := protocol.ReadResponse(a.r)
	elapsed := time.Since(start)
	switch {
	case err != nil:
	case resp == nil:
		err = errors.New("agtd closed the connection")
	case resp.Error != nil:
		err = errors.New(resp.Error.Message)
	case resp.Attached == nil:
		err = fmt.Errorf("unexpected response to attach")
	case frames && !resp.Attached.HasFeature(protocol.CapAttachFrames):
		err = fmt.Errorf("the daemon did not grant %s", protocol.CapAttachFrames)
	}
	if err != nil {
		s.Close()
		return nil, 0, fmt.Errorf("attach to %s: %w", id, err)
	}
	s.SetDeadline(time.Time{})
	a.snapshot = resp.Attached.Snapshot
	return a, elapsed, nil
}
