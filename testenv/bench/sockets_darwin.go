package bench

import "golang.org/x/sys/unix"

// tcpSocketCount reports the kernel's count of TCP sockets, including those
// in TIME_WAIT.
func tcpSocketCount() (int, bool) {
	n, err := unix.SysctlUint32("net.inet.tcp.pcbcount")
	return int(n), err == nil
}
