package netem

import (
	"bytes"
	"io"
	"net"
	"strconv"
	"testing"
	"time"
)

// echoServer echoes every connection back to the sender.
func echoServer(t *testing.T) net.Listener {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() { io.Copy(c, c); c.Close() }()
		}
	}()
	return ln
}

func TestProxyAddsRoundTripLatency(t *testing.T) {
	const rtt = 60 * time.Millisecond
	px, err := NewProxy(echoServer(t).Addr().String(), Profile{RTT: rtt})
	if err != nil {
		t.Fatal(err)
	}
	defer px.Close()

	c, err := net.Dial("tcp", px.Addr())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	buf := make([]byte, 4)
	for i := range 3 {
		start := time.Now()
		c.Write([]byte("ping"))
		if _, err := io.ReadFull(c, buf); err != nil {
			t.Fatal(err)
		}
		got := time.Since(start)
		if got < rtt || got > rtt+40*time.Millisecond {
			t.Errorf("round trip %d took %v, want ~%v", i, got, rtt)
		}
	}
}

func TestProxyLimitsBandwidth(t *testing.T) {
	const bw = 2 << 20 // 2 MiB/s
	const size = 1 << 20
	px, err := NewProxy(echoServer(t).Addr().String(), Profile{Bandwidth: bw})
	if err != nil {
		t.Fatal(err)
	}
	defer px.Close()

	c, err := net.Dial("tcp", px.Addr())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	payload := bytes.Repeat([]byte("x"), size)
	start := time.Now()
	go c.Write(payload)
	got := make([]byte, size)
	if _, err := io.ReadFull(c, got); err != nil {
		t.Fatal(err)
	}
	elapsed := time.Since(start)

	// Echo goes through the link twice but both directions pipeline, so
	// expect ~size/bw overall.
	want := time.Duration(float64(size) / bw * float64(time.Second))
	if elapsed < want*9/10 || elapsed > want*3/2 {
		t.Errorf("1 MiB at 2 MiB/s took %v, want ~%v", elapsed, want)
	}
	if !bytes.Equal(got, payload) {
		t.Error("payload corrupted in transit")
	}
}

func TestWrapListenerShapesAcceptedConns(t *testing.T) {
	inner, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	const rtt = 50 * time.Millisecond
	ln, err := WrapListener(inner, Profile{RTT: rtt})
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	go func() {
		s, err := ln.Accept()
		if err != nil {
			return
		}
		// The server must see the client's real TCP address (FTP servers
		// match the data connection's IP against the control connection's).
		if a, ok := s.RemoteAddr().(*net.TCPAddr); !ok || !a.IP.IsLoopback() {
			t.Errorf("server sees remote addr %v (%T), want a loopback *net.TCPAddr", s.RemoteAddr(), s.RemoteAddr())
		}
		io.Copy(s, s)
		s.Close()
	}()

	c, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	start := time.Now()
	c.Write([]byte("hi"))
	buf := make([]byte, 2)
	if _, err := io.ReadFull(c, buf); err != nil {
		t.Fatal(err)
	}
	if got := time.Since(start); got < rtt {
		t.Errorf("round trip %v, want >= %v", got, rtt)
	}
}

func TestUnshapedPassesEOF(t *testing.T) {
	px, err := NewProxy(echoServer(t).Addr().String(), Local)
	if err != nil {
		t.Fatal(err)
	}
	defer px.Close()
	c, err := net.Dial("tcp", px.Addr())
	if err != nil {
		t.Fatal(err)
	}
	c.Write([]byte("done"))
	c.(*net.TCPConn).CloseWrite()
	got, err := io.ReadAll(c)
	if err != nil || string(got) != "done" {
		t.Errorf("got %q, %v — half-close not propagated", got, err)
	}
}

// Regression test: a server's wildcard-bound listener ("[::]:P") whose
// port number is also held on 127.0.0.1 by another socket. Dialing
// 127.0.0.1:P would reach that other socket, so a wrapper that dials the
// server's listener hangs; WrapListener must still deliver the connection.
func TestWrapListenerSurvivesShadowedWildcardPort(t *testing.T) {
	inner, err := net.Listen("tcp", "[::]:0")
	if err != nil {
		t.Skipf("no IPv6 wildcard listener: %v", err)
	}
	port := inner.Addr().(*net.TCPAddr).Port
	decoy, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
	if err != nil {
		t.Skipf("OS refused to shadow the port (%v) — collision can't occur here", err)
	}
	defer decoy.Close()

	ln, err := WrapListener(inner, Local)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	accepted := make(chan error, 1)
	go func() {
		s, err := ln.Accept()
		if err == nil {
			s.Close()
		}
		accepted <- err
	}()
	c, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	select {
	case err := <-accepted:
		if err != nil {
			t.Fatalf("accept: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("server never received the connection — routed to the shadowing socket")
	}
}
