// Package transporttest has test helpers for the QUIC transport: a UDP
// relay that stands in for the network between a client and a daemon, so
// tests can make that network fail, change, or come back.
package transporttest

import (
	"errors"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// Relay forwards UDP datagrams between clients and one server, like a NAT:
// each client address gets its own upstream socket, so the server sees one
// address per client. Tests use it to simulate the network:
//
//   - Drop black-holes everything in both directions, as a dead network
//     or a closed laptop lid does.
//   - BlockNew drops datagrams from client addresses it has not seen, so
//     existing connections keep working while new ones cannot be made.
//   - Rebind gives every client a new upstream socket, so the server sees
//     each client arrive from a new address mid-connection, as after NAT
//     rebinding or a switch from Wi-Fi to cellular behind the same NAT.
//   - Delay holds every datagram for a one-way delay in each direction,
//     so a test runs at a distant host's round trip on loopback.
//
// Each upstream socket has one goroutine copying the server's datagrams
// back; it ends when its socket is closed by Rebind or Close. One more
// delivers delayed datagrams in order; it ends at Close.
type Relay struct {
	front  *net.UDPConn
	server *net.UDPAddr

	drop     atomic.Bool
	blockNew atomic.Bool
	delay    atomic.Int64 // nanoseconds
	queue    chan delayed
	quit     chan struct{}

	mu      sync.Mutex
	clients map[string]*relayClient
	closed  bool
	wg      sync.WaitGroup
}

// delayed is a datagram waiting for its delivery time.
type delayed struct {
	due   time.Time
	data  []byte
	write func([]byte)
}

// delayQueue bounds the datagrams in flight through the delay; past it they
// are lost, as a full queue loses them.
const delayQueue = 8192

type relayClient struct {
	addr *net.UDPAddr
	back *net.UDPConn
}

// NewRelay starts a relay to serverAddr on a fresh loopback port. It is
// closed when the test ends.
func NewRelay(t testing.TB, serverAddr string) *Relay {
	t.Helper()
	server, err := net.ResolveUDPAddr("udp", serverAddr)
	if err != nil {
		t.Fatal(err)
	}
	front, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	r := &Relay{front: front, server: server, clients: make(map[string]*relayClient), queue: make(chan delayed, delayQueue), quit: make(chan struct{})}
	r.wg.Add(2)
	go r.forward()
	go r.deliver()
	t.Cleanup(r.Close)
	return r
}

// Addr is where clients send to.
func (r *Relay) Addr() string { return r.front.LocalAddr().String() }

// Drop starts (true) or stops (false) black-holing all traffic.
func (r *Relay) Drop(drop bool) { r.drop.Store(drop) }

// BlockNew starts (true) or stops (false) dropping datagrams from client
// addresses the relay has not forwarded for before.
func (r *Relay) BlockNew(block bool) { r.blockNew.Store(block) }

// Delay sets the one-way delay in each direction (0 for none). Datagrams
// keep their order through it.
func (r *Relay) Delay(d time.Duration) { r.delay.Store(int64(d)) }

// later writes data now, or after the delay through the queue.
func (r *Relay) later(data []byte, write func([]byte)) {
	d := time.Duration(r.delay.Load())
	if d <= 0 {
		write(data)
		return
	}
	select {
	case r.queue <- delayed{due: time.Now().Add(d), data: append([]byte(nil), data...), write: write}:
	default:
	}
}

// deliver writes delayed datagrams once their time comes, in order.
func (r *Relay) deliver() {
	defer r.wg.Done()
	for {
		select {
		case d := <-r.queue:
			if wait := time.Until(d.due); wait > 0 {
				select {
				case <-time.After(wait):
				case <-r.quit:
					return
				}
			}
			d.write(d.data)
		case <-r.quit:
			return
		}
	}
}

// Rebind moves every client to a new upstream socket (a new source port as
// the server sees it). Datagrams in flight to the old sockets are lost.
func (r *Relay) Rebind() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, c := range r.clients {
		back, err := net.DialUDP("udp", nil, r.server)
		if err != nil {
			return err
		}
		old := c.back
		c.back = back
		r.wg.Add(1)
		go r.backward(c, back)
		old.Close()
	}
	return nil
}

// Close stops the relay and waits for its goroutines.
func (r *Relay) Close() {
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return
	}
	r.closed = true
	close(r.quit)
	r.front.Close()
	for _, c := range r.clients {
		c.back.Close()
	}
	r.mu.Unlock()
	r.wg.Wait()
}

// forward copies client datagrams to the server until the relay closes.
func (r *Relay) forward() {
	defer r.wg.Done()
	buf := make([]byte, 65536)
	for {
		n, from, err := r.front.ReadFromUDP(buf)
		if err != nil {
			return
		}
		if r.drop.Load() {
			continue
		}
		c := r.client(from)
		if c == nil {
			continue
		}
		r.mu.Lock()
		back := c.back
		r.mu.Unlock()
		r.later(buf[:n], func(data []byte) { back.Write(data) })
	}
}

// client returns the relay's entry for a client address, creating one
// unless new clients are blocked or the relay is closed.
func (r *Relay) client(from *net.UDPAddr) *relayClient {
	r.mu.Lock()
	defer r.mu.Unlock()
	if c, ok := r.clients[from.String()]; ok {
		return c
	}
	if r.closed || r.blockNew.Load() {
		return nil
	}
	back, err := net.DialUDP("udp", nil, r.server)
	if err != nil {
		return nil
	}
	c := &relayClient{addr: from, back: back}
	r.clients[from.String()] = c
	r.wg.Add(1)
	go r.backward(c, back)
	return c
}

// backward copies the server's datagrams on back to the client until back
// is closed.
func (r *Relay) backward(c *relayClient, back *net.UDPConn) {
	defer r.wg.Done()
	buf := make([]byte, 65536)
	for {
		n, err := back.Read(buf)
		if errors.Is(err, net.ErrClosed) {
			return
		}
		if err != nil {
			// A connected UDP socket reports ICMP errors (the server
			// restarting, say) on its next read; the relay carries on.
			continue
		}
		if !r.drop.Load() {
			r.later(buf[:n], func(data []byte) { r.front.WriteToUDP(data, c.addr) })
		}
	}
}
