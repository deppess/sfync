package watcher

import (
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/deppess/sfync/internal/config"
	"github.com/deppess/sfync/internal/syncignore"
	"github.com/deppess/sfync/internal/transfer"
)

// UploadQueue manages sequential file uploads with retry logic
type UploadQueue struct {
	queue      chan *uploadTask
	done       chan struct{} // closed by Stop to unblock Enqueue and Start
	wg         sync.WaitGroup
	profiles   map[string]*config.Profile
	profilesMu sync.RWMutex
	pool       *transfer.ConnectionPool
}

type uploadTask struct {
	profileName string
	filePath    string
}

// NewUploadQueue creates a new upload queue
func NewUploadQueue(profiles map[string]*config.Profile, pool *transfer.ConnectionPool) *UploadQueue {
	return &UploadQueue{
		queue:    make(chan *uploadTask, 100),
		done:     make(chan struct{}),
		profiles: profiles,
		pool:     pool,
	}
}

// Enqueue adds a file to the upload queue.
// Drops the task silently if Stop has been called.
func (q *UploadQueue) Enqueue(profileName, filePath string) {
	// Warn if queue is getting full (80% capacity)
	queueLen := len(q.queue)
	queueCap := cap(q.queue)
	if queueLen >= queueCap*4/5 {
		fmt.Fprintf(os.Stderr, "Warning: Upload queue is %d%% full (%d/%d)\n",
			(queueLen*100)/queueCap, queueLen, queueCap)
	}

	select {
	case q.queue <- &uploadTask{profileName: profileName, filePath: filePath}:
	case <-q.done:
		// queue stopped — drop task rather than panic on closed channel
	}
}

// Start starts processing the upload queue
func (q *UploadQueue) Start(onSuccess func(profileName, filePath string), onError func(profileName, filePath string, err error, failCount int)) {
	q.wg.Add(1)
	go func() {
		defer q.wg.Done()
		for {
			select {
			case task := <-q.queue:
				q.processUpload(task, onSuccess, onError)
			case <-q.done:
				return
			}
		}
	}()
}

// processUpload handles uploading a single file with retry logic
func (q *UploadQueue) processUpload(task *uploadTask, onSuccess func(string, string), onError func(string, string, error, int)) {
	q.profilesMu.RLock()
	profile, exists := q.profiles[task.profileName]
	q.profilesMu.RUnlock()

	if !exists {
		fmt.Fprintf(os.Stderr, "Error: Profile '%s' not found\n", task.profileName)
		return
	}

	// Get absolute paths
	absFile, err := filepath.Abs(task.filePath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: Cannot resolve file path: %v\n", err)
		return
	}

	absContext, err := filepath.Abs(profile.Context)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: Cannot resolve context path: %v\n", err)
		return
	}

	// Calculate relative path
	var relPath string
	if strings.HasPrefix(absFile, absContext+"/") {
		relPath = strings.TrimPrefix(absFile, absContext+"/")
	} else if absFile == absContext {
		relPath = filepath.Base(absFile)
	} else {
		fmt.Fprintf(os.Stderr, "Error: File '%s' not within context '%s'\n", absFile, absContext)
		return
	}

	// Check .syncignore
	patterns, err := syncignore.Load(absContext)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Warning: Failed to load .syncignore: %v\n", err)
	}

	if syncignore.ShouldIgnore(relPath, patterns) {
		fmt.Fprintf(os.Stderr, "Ignored: %s (matched .syncignore)\n", relPath)
		return
	}

	// Build remote path (always forward-slash via path.Join)
	remotePath := path.Join(profile.RemotePath, filepath.ToSlash(relPath))

	// Retry logic: 3 attempts with exponential backoff (1s, 2s, 4s)
	maxRetries := 3
	delays := []time.Duration{1 * time.Second, 2 * time.Second, 4 * time.Second}

	var lastErr error
	for attempt := 0; attempt < maxRetries; attempt++ {
		client, connErr := q.pool.Get(task.profileName)
		if connErr != nil {
			lastErr = connErr
			if attempt < maxRetries-1 {
				fmt.Fprintf(os.Stderr, "Connection failed (attempt %d/%d): %s - %v\n", attempt+1, maxRetries, relPath, connErr)
				select {
				case <-time.After(delays[attempt]):
				case <-q.done:
					return
				}
			}
			continue
		}

		// Use resume for large files (mirrors mirror.go behaviour)
		var uploadErr error
		if fi, statErr := os.Stat(absFile); statErr == nil && fi.Size() >= transfer.ResumeThreshold {
			uploadErr = client.UploadResume(absFile, remotePath)
		} else {
			uploadErr = client.Upload(absFile, remotePath)
		}
		if uploadErr == nil {
			onSuccess(task.profileName, relPath)
			return
		}

		lastErr = uploadErr

		if attempt < maxRetries-1 {
			fmt.Fprintf(os.Stderr, "Upload failed (attempt %d/%d): %s - %v\n", attempt+1, maxRetries, relPath, uploadErr)
			select {
			case <-time.After(delays[attempt]):
			case <-q.done:
				return
			}
		}
	}

	onError(task.profileName, relPath, lastErr, maxRetries)
}

// Stop shuts down the queue processor and waits for any in-flight upload to
// complete before returning. Safe to call once.
func (q *UploadQueue) Stop() {
	close(q.done)
	q.wg.Wait()
}

// UpdateProfiles atomically replaces the profile map used by the queue.
func (q *UploadQueue) UpdateProfiles(profiles map[string]*config.Profile) {
	q.profilesMu.Lock()
	defer q.profilesMu.Unlock()
	q.profiles = profiles
}
