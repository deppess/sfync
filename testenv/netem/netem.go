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
	"strconv"
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
)

// Presets lists the predefined profiles by name.
var Presets = map[string]Profile{"local": Local, "lan": LAN, "wan": WAN, "slow": Slow}

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
// directions finish, then closes both.
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

// shapedListener fronts an inner listener with a shaped one: clients connect
// to the outer address, and Accept returns the server end of a loopback
// connection to the inner listener, with a Pipe shaping traffic between the
// two. The server sees an ordinary *net.TCPConn.
type shapedListener struct {
	outer, inner net.Listener
	profile      Profile
	mu           sync.Mutex // serializes the dial/accept pairing on inner
}

// WrapListener returns a listener whose Addr() clients should dial, with
// traffic to inner's server shaped by p. Used for servers that hand out
// listeners themselves (e.g. FTP passive data ports).
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
	l.mu.Lock()
	defer l.mu.Unlock()
	dialed, err := net.Dial("tcp", loopbackAddr(l.inner.Addr()))
	if err != nil {
		client.Close()
		return nil, err
	}
	server, err := l.inner.Accept()
	if err != nil {
		client.Close()
		dialed.Close()
		return nil, err
	}
	go Pipe(client, dialed, l.profile)
	return server, nil
}

func (l *shapedListener) Close() error {
	return errors.Join(l.outer.Close(), l.inner.Close())
}

func (l *shapedListener) Addr() net.Addr { return l.outer.Addr() }

var _ io.Closer = (*Proxy)(nil)

// loopbackAddr returns a dialable address for a listener's Addr: a listener
// bound to the unspecified address ("[::]:port", "0.0.0.0:port") is dialed
// as 127.0.0.1:port. Dialing "[::]" would connect over IPv6 ::1, and servers
// that require the data connection to come from the same IP as the control
// connection (FTP) would then reject it.
func loopbackAddr(a net.Addr) string {
	tcp, ok := a.(*net.TCPAddr)
	if !ok || !tcp.IP.IsUnspecified() {
		return a.String()
	}
	return net.JoinHostPort("127.0.0.1", strconv.Itoa(tcp.Port))
}
