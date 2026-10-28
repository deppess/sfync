package bench

import (
	"testing"
	"time"
)

// socketDrainThreshold is the TCP socket count drainSockets waits to get
// under — well below macOS's ~16k ephemeral ports.
const socketDrainThreshold = 6000

// drainSockets waits, with the benchmark timer stopped, until the kernel's
// TCP socket count drops below socketDrainThreshold.
//
// FTP opens a TCP connection per file, so a cold FTP sync on loopback opens
// connections faster than macOS recycles them (each closed connection holds
// an ephemeral port for 30s in TIME_WAIT). Back-to-back iterations then
// exhaust the ~16k ephemeral ports and dials fail with "can't assign
// requested address" — a property of benchmarking at loopback speed, not
// of sfync, which never approaches that rate over a real network. Pacing
// here, outside the timer, leaves the measured protocol behavior untouched.
func drainSockets(b *testing.B) {
	b.Helper()
	n, ok := tcpSocketCount()
	if !ok || n < socketDrainThreshold {
		return
	}
	b.StopTimer()
	defer b.StartTimer()
	deadline := time.Now().Add(90 * time.Second)
	for n >= socketDrainThreshold && time.Now().Before(deadline) {
		time.Sleep(500 * time.Millisecond)
		n, _ = tcpSocketCount()
	}
}
