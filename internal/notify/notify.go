// Package notify sends desktop notifications. The actual delivery
// mechanism is OS-specific (see notify_darwin.go / notify_other.go); this
// file holds only the shared, OS-agnostic API.
package notify

import "os"

type Urgency string

const (
	UrgencyNormal   Urgency = "normal"
	UrgencyCritical Urgency = "critical"
)

func Success(title, message string) { Send("✓ "+title, message, UrgencyNormal) }
func Error(title, message string)   { Send("✗ "+title, message, UrgencyCritical) }
func Warning(title, message string) { Send("⚠ "+title, message, UrgencyNormal) }
func Info(title, message string)    { Send(title, message, UrgencyNormal) }

// disabled reports whether desktop notifications are turned off by setting
// SFYNC_NO_NOTIFY to any non-empty value — for scripts, CI and tests, where
// the terminal output is enough and a notification would be noise.
func disabled() bool { return os.Getenv("SFYNC_NO_NOTIFY") != "" }
