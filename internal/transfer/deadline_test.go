package transfer

import (
	"net"
	"sync"
	"testing"
	"time"
)

// recordingConn is a net.Conn stub that records the last deadline set.
type recordingConn struct {
	net.Conn
	mu       sync.Mutex
	deadline time.Time
}

func (r *recordingConn) SetDeadline(t time.Time) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.deadline = t
	return nil
}

func (r *recordingConn) get() time.Time {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.deadline
}

func TestDeadlineTrackerKeepsLatestDeadline(t *testing.T) {
	conn := &recordingConn{}
	tr := newDeadlineTracker(conn)

	endLong := tr.begin(time.Hour)
	longDeadline := conn.get()

	// A short sibling operation must not shorten the long one's deadline...
	endShort := tr.begin(time.Second)
	if got := conn.get(); !got.Equal(longDeadline) {
		t.Fatalf("short op shortened deadline: got %v, want %v", got, longDeadline)
	}

	// ...and finishing it must not clear the long one's deadline.
	endShort()
	if got := conn.get(); !got.Equal(longDeadline) {
		t.Fatalf("finishing short op changed deadline: got %v, want %v", got, longDeadline)
	}

	endLong()
	if got := conn.get(); !got.IsZero() {
		t.Fatalf("deadline not cleared with no ops in flight: got %v", got)
	}
}

func TestDeadlineTrackerFallsBackToRemainingOp(t *testing.T) {
	conn := &recordingConn{}
	tr := newDeadlineTracker(conn)

	endShort := tr.begin(time.Second)
	shortDeadline := conn.get()
	endLong := tr.begin(time.Hour)

	// Once the long op ends, the still-running short op keeps its protection.
	endLong()
	if got := conn.get(); !got.Equal(shortDeadline) {
		t.Fatalf("got %v, want remaining op's deadline %v", got, shortDeadline)
	}

	// end is idempotent — calling it twice must not disturb anything.
	endLong()
	if got := conn.get(); !got.Equal(shortDeadline) {
		t.Fatalf("double end changed deadline: got %v", got)
	}
	endShort()
	if got := conn.get(); !got.IsZero() {
		t.Fatalf("deadline not cleared: got %v", got)
	}
}

func TestDeadlineTrackerConcurrent(t *testing.T) {
	conn := &recordingConn{}
	tr := newDeadlineTracker(conn)

	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			end := tr.begin(time.Duration(i+1) * time.Second)
			end()
		}(i)
	}
	wg.Wait()
	if got := conn.get(); !got.IsZero() {
		t.Fatalf("deadline not cleared after all ops ended: got %v", got)
	}
}
