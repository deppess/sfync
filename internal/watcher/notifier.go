package watcher

import (
	"fmt"
	"sync"
	"time"

	"github.com/deppess/sfync/internal/notify"
)

const (
	batchWindow    = 30 * time.Second
	batchThreshold = 5
)

// Notifier manages notification batching for uploads
type Notifier struct {
	successCount   map[string]int
	lastNotifyTime map[string]time.Time
	errorCount     map[string]int
	mutex          sync.Mutex
}

// NewNotifier creates a new notifier with batching
func NewNotifier() *Notifier {
	return &Notifier{
		successCount:   make(map[string]int),
		lastNotifyTime: make(map[string]time.Time),
		errorCount:     make(map[string]int),
	}
}

// NotifySuccess handles successful upload notifications with batching
func (n *Notifier) NotifySuccess(profileName, relPath string) {
	n.mutex.Lock()
	defer n.mutex.Unlock()

	// Increment count
	n.successCount[profileName]++
	count := n.successCount[profileName]

	timeSinceLastNotify := time.Since(n.lastNotifyTime[profileName])

	// Determine if we should notify
	shouldNotify := false
	message := ""

	if count == 1 {
		// First upload since last batch — show filename
		shouldNotify = true
		message = fmt.Sprintf("%s → %s", relPath, profileName)
	} else if count >= batchThreshold {
		// Hit threshold — show count
		shouldNotify = true
		message = fmt.Sprintf("%d files → %s", count, profileName)
	} else if timeSinceLastNotify >= batchWindow {
		// Time window expired - show count
		shouldNotify = true
		message = fmt.Sprintf("%d files → %s", count, profileName)
	}

	if shouldNotify {
		notify.Success("Auto-synced", message)
		n.lastNotifyTime[profileName] = time.Now()
		n.successCount[profileName] = 0 // Reset counter
	}
}

// NotifyError handles error notifications with backoff
// Shows: 1st failure, then every 5th, then every 10th
func (n *Notifier) NotifyError(profileName, relPath string, err error) {
	n.mutex.Lock()
	defer n.mutex.Unlock()

	// Increment error count
	n.errorCount[profileName]++
	count := n.errorCount[profileName]

	// Determine if we should notify based on backoff
	shouldNotify := false
	if count == 1 {
		// First failure
		shouldNotify = true
	} else if count <= 10 && count%5 == 0 {
		// 5th, 10th failure
		shouldNotify = true
	} else if count > 10 && count%10 == 0 {
		// Every 10th failure after 10
		shouldNotify = true
	}

	if shouldNotify {
		var message string
		if count == 1 {
			message = fmt.Sprintf("%s → %s\n%v", relPath, profileName, err)
		} else {
			message = fmt.Sprintf("%s (failed %d times)\n%v", relPath, count, err)
		}
		notify.Error("Auto-sync failed", message)
	}
}

// ResetErrorCount resets the error count for a profile (call on success)
func (n *Notifier) ResetErrorCount(profileName string) {
	n.mutex.Lock()
	defer n.mutex.Unlock()
	n.errorCount[profileName] = 0
}
