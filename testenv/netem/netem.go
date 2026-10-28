// Package netem emulates network conditions (latency and bandwidth) for
// sfync's tests and benchmarks, entirely in userspace over real loopback TCP
// — no root, no system firewall/dummynet state.
//
// Each direction of a shaped connection behaves like a link: bytes leave at
// most at the profile's bandwidth (serialization delay) and then arrive one
// one-way delay (RTT/2) later. Pacing the reader at link rate bounds what's
// in flight to roughly the bandwidth-delay product, like a real path,
// instead of buffering unboundedly ahead.
package netem

import (
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"time"
)

// Profile describes emulated network conditions.
type Profile struct {
	Name      string
	RTT       time.Duration // round-trip time; each direction gets RTT/2
	Bandwidth int64         // bytes per second per direction; 0 = unlimited
}

// Unshaped reports whether p adds no delay or rate limit.
func (p Profile) Unshaped() bool { return p.RTT == 0 && p.Bandwidth == 0 }

func (p Profile) String() string {
	if p.Name != "" {
		return p.Name
	}
	return fmt.Sprintf("rtt=%s,bw=%d", p.RTT, p.Bandwidth)
}

// Mbps converts megabits per second to the bytes/second Profile.Bandwidth
// expects.
func Mbps(n float64) int64 { return int64(n * 1e6 / 8) }

// Presets used by the benchmark matrix. RTTs are representative, not
// measurements of any particular network.
var (
	Local = Profile{Name: "local"}                                                  // plain loopback
	LAN   = Profile{Name: "lan", RTT: 1 * time.Millisecond, Bandwidth: Mbps(1000)}  // wired LAN
	WAN   = Profile{Name: "wan", RTT: 40 * time.Millisecond, Bandwidth: Mbps(100)}  // typical remote host
	Slow  = Profile{Name: "slow", RTT: 120 * time.Millisecond, Bandwidth: Mbps(10)} // distant / constrained
	// FatPipe is a fast link with real latency (fiber, datacenter to
	// datacenter): a large bandwidth-delay product (5 MB), where in-flight
	// data limits — not bandwidth — decide throughput.
	FatPipe = Profile{Name: "fatpipe", RTT: 40 * time.Millisecond, Bandwidth: Mbps(1000)}
)

// Presets lists the predefined profiles by name.
var Presets = map[string]Profile{"local": Local, "lan": LAN, "wan": WAN, "slow": Slow, "fatpipe": FatPipe}

const chunkSize = 32 * 1024

type packet struct {
	data []byte
	due  time.Time
}

// shapeOneWay copies src to dst under p, then half-closes dst's write side
// (or closes it) so the peer sees EOF.
func shapeOneWay(dst, src net.Conn, p Profile) {
	defer closeWrite(dst)

	oneWay := p.RTT / 2
	queue := make(chan packet, 4096)
	done := make(chan struct{})

	go func() { // delivery side: release each chunk when it "arrives"
		defer close(done)
		for pkt := range queue {
			if d := time.Until(pkt.due); d > 0 {
				time.Sleep(d)
			}
			if _, err := dst.Write(pkt.data); err != nil {
				for range queue { // drain so the reader never blocks
				}
				return
			}
		}
	}()

	var linkFree time.Time // when the emulated link finishes sending
	buf := make([]byte, chunkSize)
	for {
		n, err := src.Read(buf)
		if n > 0 {
			now := time.Now()
			start := now
			if linkFree.After(start) {
				start = linkFree
			}
			sent := start
			if p.Bandwidth > 0 {
				sent = start.Add(time.Duration(float64(n) / float64(p.Bandwidth) * float64(time.Second)))
			}
			linkFree = sent
			data := make([]byte, n)
			copy(data, buf[:n])
			queue <- packet{data: data, due: sent.Add(oneWay)}
			// Pace reads at link rate so in-flight data stays near the
			// bandwidth-delay product.
			if d := time.Until(sent); d > 0 {
				time.Sleep(d)
			}
		}
		if err != nil {
			break
		}
	}
	close(queue)
	<-done
}

func closeWrite(c net.Conn) {
	if cw, ok := c.(interface{ CloseWrite() error }); ok {
		cw.CloseWrite()
		return
	}
	c.Close()
}

// Pipe shapes traffic between a and b in both directions until both
// directions finish, then closes both — always gracefully: each side gets
// a FIN once the other has finished sending, so a peer that half-closed and
// is still reading receives every byte before EOF. (Resetting connections
// to dodge TIME_WAIT would be faster to recycle but can discard unread
// data; ephemeral-port pressure is instead handled by pacing in the
// benchmarks — see bench.drainSockets.)
func Pipe(a, b net.Conn, p Profile) {
	var wg sync.WaitGroup
	wg.Go(func() { shapeOneWay(b, a, p) })
	wg.Go(func() { shapeOneWay(a, b, p) })
	wg.Wait()
	a.Close()
	b.Close()
}

// Proxy is a shaped TCP proxy listening on 127.0.0.1, forwarding every
// connection to a target address.
type Proxy struct {
	ln      net.Listener
	target  string
	profile Profile
	wg      sync.WaitGroup
	mu      sync.Mutex
	conns   map[net.Conn]struct{}
}

// NewProxy starts a proxy to target under profile p.
func NewProxy(target string, p Profile) (*Proxy, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	px := &Proxy{ln: ln, target: target, profile: p, conns: make(map[net.Conn]struct{})}
	px.wg.Go(px.serve)
	return px, nil
}

// Addr is the address clients should dial.
func (px *Proxy) Addr() string { return px.ln.Addr().String() }

func (px *Proxy) serve() {
	for {
		client, err := px.ln.Accept()
		if err != nil {
			return
		}
		upstream, err := net.Dial("tcp", px.target)
		if err != nil {
			client.Close()
			continue
		}
		px.track(client, upstream)
		px.wg.Go(func() {
			Pipe(client, upstream, px.profile)
			px.untrack(client, upstream)
		})
	}
}

func (px *Proxy) track(cs ...net.Conn) {
	px.mu.Lock()
	defer px.mu.Unlock()
	for _, c := range cs {
		px.conns[c] = struct{}{}
	}
}

func (px *Proxy) untrack(cs ...net.Conn) {
	px.mu.Lock()
	defer px.mu.Unlock()
	for _, c := range cs {
		delete(px.conns, c)
	}
}

// Close stops accepting, tears down open connections, and waits for all
// proxy goroutines to finish.
func (px *Proxy) Close() error {
	err := px.ln.Close()
	px.mu.Lock()
	for c := range px.conns {
		c.Close()
	}
	px.mu.Unlock()
	px.wg.Wait()
	return err
}

// shapedListener stands in for a server's own listener (e.g. an FTP
// passive data port). Clients connect to outer; Accept hands the server one
// end of an in-memory net.Pipe — reporting the client's real TCP addresses,
// so address-based checks (FTP's data-connection IP match) still work —
// with a Pipe shaping traffic between the client and the other end.
//
// No second TCP hop: each client connection costs exactly one real TCP
// connection, as it would without shaping. An earlier design re-dialed the
// server over loopback per connection, doubling ephemeral-port use; at
// FTP's one-connection-per-file rate that exhausted macOS's ~16k ephemeral
// ports (each closed connection holds one in TIME_WAIT for 30s). Dialing
// the server's own listener was also unreliable: FTP servers bind passive
// ports on the wildcard address ("[::]:P"), and any other socket holding
// 127.0.0.1:P captured the dial. inner is kept only to be closed.
//
// Limitation: net.Pipe has no half-close, so when the client half-closes,
// the server end is closed fully. That suits FTP data connections, which
// carry data one way per transfer.
type shapedListener struct {
	outer, inner net.Listener
	profile      Profile
}

// WrapListener returns a listener to hand to a server in place of inner:
// clients dial its Addr(), and traffic between them and the server is
// shaped by p. Used for servers that create listeners themselves (FTP
// passive data ports).
func WrapListener(inner net.Listener, p Profile) (net.Listener, error) {
	outer, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	return &shapedListener{outer: outer, inner: inner, profile: p}, nil
}

func (l *shapedListener) Accept() (net.Conn, error) {
	client, err := l.outer.Accept()
	if err != nil {
		return nil, err
	}
	near, server := net.Pipe()
	go Pipe(client, near, l.profile)
	return &addrConn{Conn: server, local: client.LocalAddr(), remote: client.RemoteAddr()}, nil
}

func (l *shapedListener) Close() error {
	return errors.Join(l.outer.Close(), l.inner.Close())
}

// addrConn overrides a connection's reported addresses.
type addrConn struct {
	net.Conn
	local, remote net.Addr
}

func (c *addrConn) LocalAddr() net.Addr  { return c.local }
func (c *addrConn) RemoteAddr() net.Addr { return c.remote }

func (l *shapedListener) Addr() net.Addr { return l.outer.Addr() }

var _ io.Closer = (*Proxy)(nil)
