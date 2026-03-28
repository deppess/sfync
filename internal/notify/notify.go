package notify

import (
	"os/exec"
)

type Urgency string

const (
	UrgencyNormal   Urgency = "normal"
	UrgencyCritical Urgency = "critical"
)

// Send displays a desktop notification using notify-send
func Send(title, message string, urgency Urgency) {
	exec.Command("notify-send", "-u", string(urgency), title, message).Run()
}

func Success(title, message string) { Send("✓ "+title, message, UrgencyNormal) }
func Error(title, message string)   { Send("✗ "+title, message, UrgencyCritical) }
func Warning(title, message string) { Send("⚠ "+title, message, UrgencyNormal) }
func Info(title, message string)    { Send(title, message, UrgencyNormal) }
