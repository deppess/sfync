package notify

import (
	"os/exec"
	"runtime"
	"strings"
)

type Urgency string

const (
	UrgencyNormal   Urgency = "normal"
	UrgencyCritical Urgency = "critical"
)

// RequiredDep returns the external command this package needs to send
// notifications on the current OS, for use with deps.CheckRequired.
func RequiredDep() string {
	if runtime.GOOS == "darwin" {
		return "osascript"
	}
	return "notify-send"
}

// Send displays a desktop notification using notify-send on Linux or
// osascript (Notification Center) on macOS.
func Send(title, message string, urgency Urgency) {
	if runtime.GOOS == "darwin" {
		script := "display notification " + quoteAppleScript(message) + " with title " + quoteAppleScript(title)
		exec.Command("osascript", "-e", script).Run()
		return
	}
	exec.Command("notify-send", "-u", string(urgency), title, message).Run()
}

// quoteAppleScript escapes backslashes and double quotes so the string can
// be embedded as an AppleScript string literal.
func quoteAppleScript(s string) string {
	escaped := strings.ReplaceAll(s, `\`, `\\`)
	escaped = strings.ReplaceAll(escaped, `"`, `\"`)
	return `"` + escaped + `"`
}

func Success(title, message string) { Send("✓ "+title, message, UrgencyNormal) }
func Error(title, message string)   { Send("✗ "+title, message, UrgencyCritical) }
func Warning(title, message string) { Send("⚠ "+title, message, UrgencyNormal) }
func Info(title, message string)    { Send(title, message, UrgencyNormal) }
