//go:build !darwin

package notify

import (
	"os/exec"

	"github.com/deppess/sfync/internal/deps"
)

// Send displays a desktop notification via notify-send (Linux/libnotify).
func Send(title, message string, urgency Urgency) {
	exec.Command("notify-send", "-u", string(urgency), title, message).Run()
}

// EnsureReady verifies notify-send is on PATH.
func EnsureReady() error {
	return deps.CheckRequired("notify-send")
}

// Reinstall is a no-op on this platform — notify-send has no local state
// to refresh, unlike the embedded helper app on macOS.
func Reinstall() error { return nil }

// TestNotification is a no-op on this platform; install-daemon's darwin
// path is the only real caller, but the symbol must exist everywhere since
// cmd/install.go is compiled for every OS.
func TestNotification() error { return nil }
