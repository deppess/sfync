package transfer

import (
	"net"
	"sync"
	"time"
)

// deadlineTracker arbitrates the single read/write deadline of a connection
// shared by concurrent operations.
//
// net.Conn has exactly one deadline, but an sftpClient serves several
// goroutines at once (parallel ReadDir in walkRemote, parallel transfers in
// runTransfers). If each operation set and cleared that deadline directly,
// operations would clobber each other: a finishing small transfer would clear
// the deadline protecting a large one still in flight, and a fresh short
// deadline would cut a long transfer off early.
//
// Instead, each operation registers its own deadline, and the connection's
// deadline is always the latest among all in-flight operations — so no
// operation is ever cut short by a sibling's budget, yet a connection that
// stalls entirely is still torn down once the most generous budget runs out.
// With no operations in flight the deadline is cleared.
type deadlineTracker struct {
	mu     sync.Mutex
	conn   net.Conn
	active map[uint64]time.Time
	nextID uint64
}

func newDeadlineTracker(conn net.Conn) *deadlineTracker {
	return &deadlineTracker{conn: conn, active: make(map[uint64]time.Time)}
}

// begin registers an operation allowed to run for d and returns the function
// that unregisters it. Typical use: defer t.begin(OperationTimeout)().
func (t *deadlineTracker) begin(d time.Duration) (end func()) {
	t.mu.Lock()
	defer t.mu.Unlock()

	id := t.nextID
	t.nextID++
	t.active[id] = time.Now().Add(d)
	t.apply()

	var once sync.Once
	return func() {
		once.Do(func() {
			t.mu.Lock()
			defer t.mu.Unlock()
			delete(t.active, id)
			t.apply()
		})
	}
}

// apply sets the connection deadline to the latest registered deadline, or
// clears it when nothing is in flight. Caller must hold t.mu.
func (t *deadlineTracker) apply() {
	var latest time.Time
	for _, dl := range t.active {
		if dl.After(latest) {
			latest = dl
		}
	}
	t.conn.SetDeadline(latest) // zero time clears the deadline
}
