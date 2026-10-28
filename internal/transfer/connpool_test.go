package transfer

import (
	"errors"
	"fmt"
	"io/fs"
	"net"
	"net/textproto"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fakeConn is a pool connection that records whether it was closed.
type fakeConn struct {
	id     int
	closed atomic.Bool
}

type poolHarness struct {
	pool    *connPool[*fakeConn]
	dials   atomic.Int32
	closes  atomic.Int32
	failAt  atomic.Int32 // dial number (1-based) from which dials fail; 0 = never
	capped  atomic.Int32 // limit reported to onCap; 0 = not capped
	capHits atomic.Int32
}

func newPoolHarness(limit int) *poolHarness {
	h := &poolHarness{}
	h.pool = newConnPool(limit,
		func() (*fakeConn, error) {
			n := h.dials.Add(1)
			if f := h.failAt.Load(); f != 0 && n >= f {
				return nil, errors.New("421 too many connections")
			}
			return &fakeConn{id: int(n)}, nil
		},
		func(c *fakeConn) { c.closed.Store(true); h.closes.Add(1) },
		func(limit int) { h.capped.Store(int32(limit)); h.capHits.Add(1) },
	)
	return h
}

func TestConnPoolReusesBeforeDialing(t *testing.T) {
	h := newPoolHarness(4)
	for range 10 { // strictly sequential use
		c, err := h.pool.acquire()
		if err != nil {
			t.Fatal(err)
		}
		h.pool.release(c, false)
	}
	if got := h.dials.Load(); got != 1 {
		t.Errorf("sequential use dialed %d connections, want 1", got)
	}
}

func TestConnPoolGrowsToLimitUnderConcurrency(t *testing.T) {
	h := newPoolHarness(3)
	var held []*fakeConn
	for range 3 {
		c, err := h.pool.acquire()
		if err != nil {
			t.Fatal(err)
		}
		held = append(held, c)
	}
	if got := h.dials.Load(); got != 3 {
		t.Fatalf("dials = %d, want 3", got)
	}

	// A 4th acquire must wait (limit reached), not dial.
	got := make(chan *fakeConn)
	go func() { c, _ := h.pool.acquire(); got <- c }()
	select {
	case <-got:
		t.Fatal("acquire beyond the limit didn't wait")
	case <-time.After(50 * time.Millisecond):
	}
	h.pool.release(held[0], false)
	if c := <-got; c != held[0] {
		t.Error("waiter didn't receive the released connection")
	}
	if h.dials.Load() != 3 {
		t.Error("waiter dialed instead of reusing")
	}
}

func TestConnPoolCapsWhenServerRefusesExtraConnections(t *testing.T) {
	h := newPoolHarness(4)
	h.failAt.Store(3) // the server allows only 2

	a, _ := h.pool.acquire()
	b, _ := h.pool.acquire()
	done := make(chan error, 1)
	go func() {
		c, err := h.pool.acquire() // dial #3 fails → cap at 2 → wait
		if err == nil {
			h.pool.release(c, false)
		}
		done <- err
	}()
	time.Sleep(50 * time.Millisecond)
	h.pool.release(a, false)
	if err := <-done; err != nil {
		t.Fatalf("acquire failed instead of waiting for a free connection: %v", err)
	}
	h.pool.release(b, false)

	if h.capped.Load() != 2 || h.capHits.Load() != 1 {
		t.Errorf("onCap: limit=%d calls=%d, want limit=2 once", h.capped.Load(), h.capHits.Load())
	}
	// Once capped, it must not keep re-dialing.
	before := h.dials.Load()
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			c, err := h.pool.acquire()
			if err == nil {
				h.pool.release(c, false)
			}
		})
	}
	wg.Wait()
	if h.dials.Load() != before {
		t.Errorf("capped pool dialed again: %d -> %d", before, h.dials.Load())
	}
}

func TestConnPoolFirstDialFailureIsAnError(t *testing.T) {
	h := newPoolHarness(4)
	h.failAt.Store(1)
	if _, err := h.pool.acquire(); err == nil {
		t.Fatal("expected error when no connection can be opened at all")
	}
}

func TestConnPoolBrokenConnectionIsReplaced(t *testing.T) {
	h := newPoolHarness(1)
	c, _ := h.pool.acquire()
	h.pool.release(c, true)
	if !c.closed.Load() {
		t.Error("broken connection not closed")
	}
	c2, err := h.pool.acquire()
	if err != nil {
		t.Fatal(err)
	}
	if c2 == c {
		t.Error("broken connection was reused")
	}
	h.pool.release(c2, false)
}

func TestConnPoolCloseAll(t *testing.T) {
	h := newPoolHarness(2)
	idle, _ := h.pool.acquire()
	busy, _ := h.pool.acquire()
	h.pool.release(idle, false)

	h.pool.closeAll()
	if !idle.closed.Load() {
		t.Error("idle connection not closed by closeAll")
	}
	if busy.closed.Load() {
		t.Error("checked-out connection closed under its user")
	}
	h.pool.release(busy, false)
	if !busy.closed.Load() {
		t.Error("connection released after closeAll not closed")
	}
	if _, err := h.pool.acquire(); !errors.Is(err, errPoolClosed) {
		t.Errorf("acquire after closeAll: %v", err)
	}
}

// Many goroutines hammering the pool: no connection is ever handed to two
// holders at once, and the limit is never exceeded.
func TestConnPoolExclusiveUnderStress(t *testing.T) {
	const limit = 4
	h := newPoolHarness(limit)
	var inUse sync.Map
	var concurrent, peak atomic.Int32
	var wg sync.WaitGroup
	for range 64 {
		wg.Go(func() {
			for range 50 {
				c, err := h.pool.acquire()
				if err != nil {
					t.Error(err)
					return
				}
				if _, dup := inUse.LoadOrStore(c, true); dup {
					t.Error("connection handed to two holders")
				}
				n := concurrent.Add(1)
				for {
					p := peak.Load()
					if n <= p || peak.CompareAndSwap(p, n) {
						break
					}
				}
				concurrent.Add(-1)
				inUse.Delete(c)
				h.pool.release(c, false)
			}
		})
	}
	wg.Wait()
	if peak.Load() > limit || h.dials.Load() > limit {
		t.Errorf("limit exceeded: peak=%d dials=%d", peak.Load(), h.dials.Load())
	}
}

func TestSessionBroken(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{"success", nil, false},
		{"server reply 550 (no such file)", &textproto.Error{Code: 550, Msg: "No such file"}, false},
		{"server reply 553 through wrapError", wrapError("upload", &textproto.Error{Code: 553, Msg: "denied"}), false},
		{"421 service closing", &textproto.Error{Code: 421, Msg: "closing"}, true},
		{"network error", &net.OpError{Op: "read", Err: errors.New("connection reset")}, true},
		{"timeout", wrapError("readdir", os.ErrDeadlineExceeded), true},
		{"local file error", wrapError("download", &fs.PathError{Op: "open", Path: "/x", Err: fs.ErrPermission}), false},
		{"lost working directory", fmt.Errorf("restore: %w", errors.Join(errBrokenSession, &textproto.Error{Code: 550})), true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := sessionBroken(tc.err); got != tc.want {
				t.Errorf("sessionBroken(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}
