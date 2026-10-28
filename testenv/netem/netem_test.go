package netem

import (
	"bytes"
	"io"
	"net"
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
		if _, ok := s.(*net.TCPConn); !ok {
			t.Errorf("server side is %T, want *net.TCPConn", s)
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
