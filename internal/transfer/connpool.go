package transfer

import (
	"errors"
	"sync"
)

// errPoolClosed is returned by acquire after closeAll.
var errPoolClosed = errors.New("connection pool closed")

// connPool hands out exclusive use of up to limit connections, opening them
// lazily — only when every open connection is busy — so sequential work
// never opens more than one.
//
// It is built for protocols like FTP, where a connection carries one
// command at a time and holds per-connection state (working directory,
// transfer mode): a caller has a connection to itself from acquire to
// release, so state can't leak between concurrent operations.
//
// Servers often cap connections per user. If opening an extra connection
// fails while others are open, the pool lowers its limit to what it has
// (reporting it once via onCap) and callers wait for a free connection
// instead of failing. Only failing to open the very first connection is an
// error — exactly as with a single connection.
type connPool[C any] struct {
	mu     sync.Mutex
	cond   *sync.Cond
	idle   []C
	open   int // connections in existence: idle + checked out
	limit  int
	capped bool
	closed bool

	dial  func() (C, error)
	close func(C)
	onCap func(limit int)
}

func newConnPool[C any](limit int, dial func() (C, error), closeFn func(C), onCap func(int)) *connPool[C] {
	p := &connPool[C]{limit: max(limit, 1), dial: dial, close: closeFn, onCap: onCap}
	p.cond = sync.NewCond(&p.mu)
	return p
}

// seed adds an already-open connection (e.g. the one Connect opened to fail
// fast on bad credentials) to the pool.
func (p *connPool[C]) seed(c C) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.open++
	p.idle = append(p.idle, c)
	p.cond.Signal()
}

// acquire returns a connection for the caller's exclusive use, reusing an
// idle one, opening a new one if under the limit, or waiting. Every
// successful acquire must be paired with exactly one release.
func (p *connPool[C]) acquire() (C, error) {
	var zero C
	p.mu.Lock()
	defer p.mu.Unlock()
	for {
		if p.closed {
			return zero, errPoolClosed
		}
		if n := len(p.idle); n > 0 {
			c := p.idle[n-1]
			p.idle = p.idle[:n-1]
			return c, nil
		}
		if p.open < p.limit {
			p.open++
			p.mu.Unlock()
			c, err := p.dial() // network I/O outside the lock
			p.mu.Lock()
			if err == nil {
				if p.closed {
					p.open--
					go p.close(c)
					return zero, errPoolClosed
				}
				return c, nil
			}
			p.open--
			if p.open == 0 {
				return zero, err // no connection at all: a real failure
			}
			// The server accepts fewer connections than configured (or
			// briefly couldn't take another): stop growing and share the
			// ones that work.
			p.limit = p.open
			if !p.capped {
				p.capped = true
				if p.onCap != nil {
					p.onCap(p.limit)
				}
			}
			continue
		}
		p.cond.Wait()
	}
}

// release returns a connection acquired from the pool. A broken connection
// (its session can no longer be trusted, e.g. after a network error) is
// closed rather than reused, freeing its slot for a fresh one.
func (p *connPool[C]) release(c C, broken bool) {
	p.mu.Lock()
	if broken || p.closed {
		p.open--
		p.cond.Broadcast() // a waiter may now open a replacement
		p.mu.Unlock()
		p.close(c)
		return
	}
	p.idle = append(p.idle, c)
	p.cond.Signal()
	p.mu.Unlock()
}

// tryAcquireIdle returns an idle connection without waiting or dialing.
// busy reports whether connections exist but are all checked out.
func (p *connPool[C]) tryAcquireIdle() (c C, ok, busy bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if n := len(p.idle); n > 0 && !p.closed {
		c = p.idle[n-1]
		p.idle = p.idle[:n-1]
		return c, true, false
	}
	return c, false, p.open > 0 && !p.closed
}

// closeAll closes idle connections and marks the pool closed; connections
// still checked out are closed when released. Waiting acquires fail.
func (p *connPool[C]) closeAll() {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return
	}
	p.closed = true
	idle := p.idle
	p.idle = nil
	p.open -= len(idle)
	p.cond.Broadcast()
	p.mu.Unlock()
	for _, c := range idle {
		p.close(c)
	}
}
