package watcher

import (
	"sync"
	"time"
)

// Debouncer manages per-file debounce timers
type Debouncer struct {
	timers map[string]*time.Timer
	mutex  sync.Mutex
}

// NewDebouncer creates a new debouncer
func NewDebouncer() *Debouncer {
	return &Debouncer{
		timers: make(map[string]*time.Timer),
	}
}

// Add adds or resets a debounce timer for a file.
// If the file already has a timer, it resets it.
// When the timer expires, the callback is called.
func (d *Debouncer) Add(filePath string, delay time.Duration, callback func()) {
	d.mutex.Lock()
	defer d.mutex.Unlock()

	// If timer already exists, stop it
	if timer, exists := d.timers[filePath]; exists {
		timer.Stop()
	}

	// Create new timer
	d.timers[filePath] = time.AfterFunc(delay, func() {
		// Remove timer from map
		d.mutex.Lock()
		delete(d.timers, filePath)
		d.mutex.Unlock()

		// Call callback
		callback()
	})
}

// Stop cancels the timer for a file
func (d *Debouncer) Stop(filePath string) {
	d.mutex.Lock()
	defer d.mutex.Unlock()

	if timer, exists := d.timers[filePath]; exists {
		timer.Stop()
		delete(d.timers, filePath)
	}
}

// StopAll cancels all timers
func (d *Debouncer) StopAll() {
	d.mutex.Lock()
	defer d.mutex.Unlock()

	for _, timer := range d.timers {
		timer.Stop()
	}
	d.timers = make(map[string]*time.Timer)
}
