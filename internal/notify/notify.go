// Package notify sends desktop notifications. The actual delivery
// mechanism is OS-specific (see notify_darwin.go / notify_other.go); this
// file holds only the shared, OS-agnostic API.
package notify

type Urgency string

const (
	UrgencyNormal   Urgency = "normal"
	UrgencyCritical Urgency = "critical"
)

func Success(title, message string) { Send("✓ "+title, message, UrgencyNormal) }
func Error(title, message string)   { Send("✗ "+title, message, UrgencyCritical) }
func Warning(title, message string) { Send("⚠ "+title, message, UrgencyNormal) }
func Info(title, message string)    { Send(title, message, UrgencyNormal) }
