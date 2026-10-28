//go:build !darwin

package bench

// tcpSocketCount is unavailable here; drainSockets then doesn't wait.
func tcpSocketCount() (int, bool) { return 0, false }
