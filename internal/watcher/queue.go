package watcher

import (
	"errors"
	"fmt"
	"io/fs"
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
	clients    clientSource

	// knownDirs remembers remote directories already ensured, keyed by
	// profile + "\x00" + dir, so repeated uploads skip MkdirAll. Cleared
	// for a profile when an upload fails or a remote directory is deleted,
	// and entirely on config reload.
	knownDirs map[string]bool
	dirsMu    sync.Mutex
}

// clientSource hands out live connections per profile — satisfied by
// *transfer.ConnectionPool, and by fakes in tests.
type clientSource interface {
	Get(profileName string) (transfer.RemoteClient, error)
}

type uploadTask struct {
	profileName string
	filePath    string
}

// NewUploadQueue creates a new upload queue
func NewUploadQueue(profiles map[string]*config.Profile, pool clientSource) *UploadQueue {
	return &UploadQueue{
		queue:    make(chan *uploadTask, 100),
		done:     make(chan struct{}),
		profiles: profiles,
		clients:  pool,
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
	q.wg.Go(func() {
		for {
			select {
			case task := <-q.queue:
				q.processUpload(task, onSuccess, onError)
			case <-q.done:
				return
			}
		}
	})
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
	if after, ok := strings.CutPrefix(absFile, absContext+"/"); ok {
		relPath = after
	} else if absFile == absContext {
		relPath = filepath.Base(absFile)
	} else {
		fmt.Fprintf(os.Stderr, "Error: File '%s' not within context '%s'\n", absFile, absContext)
		return
	}

	// Never sync sfync's own in-progress transfer files — e.g. the temp
	// files a concurrent `sfync down` writes into this context.
	if transfer.IsPartialName(filepath.Base(absFile)) {
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
	for attempt := range maxRetries {
		// The file may be gone by the time its debounce fires: editors that
		// save atomically (vim's 4913 probe, `file~` backups, JetBrains'
		// ___jb_tmp___) create short-lived temp files that are renamed or
		// deleted moments later. That's not a failure — drop the task
		// instead of retrying and raising an error notification. Checked on
		// every attempt so a file deleted mid-retry is dropped too.
		//
		// With autoSyncDeletes on, a path that's gone is instead deleted
		// remotely. The decision is made here, when the debounce fires, not
		// when the event arrived: an editor's delete-then-recreate save
		// collapses into one task that finds the file present and uploads it.
		fi, statErr := os.Stat(absFile)
		missing := errors.Is(statErr, fs.ErrNotExist)
		if missing && !profile.AutoSyncDeletes {
			fmt.Fprintf(os.Stderr, "Skipped: %s (no longer exists)\n", relPath)
			return
		}
		if missing {
			// Guard against mass deletion: if the context directory itself is
			// gone (unmounted drive, moved or deleted project), every file in
			// it looks deleted. That's never something to mirror.
			if _, ctxErr := os.Stat(absContext); ctxErr != nil {
				fmt.Fprintf(os.Stderr, "Not deleting %s: context %s is unavailable\n", relPath, absContext)
				return
			}
		} else if statErr == nil && !fi.Mode().IsRegular() {
			return
		}

		client, connErr := q.clients.Get(task.profileName)
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

		if missing {
			deleted, delErr := deleteRemote(client, remotePath, relPath)
			if deleted {
				// A deleted directory invalidates remembered ones under it.
				q.forgetDirs(task.profileName)
			}
			if delErr == nil {
				if deleted {
					onSuccess(task.profileName, relPath+" (deleted)")
				}
				return
			}
			lastErr = delErr
			if attempt < maxRetries-1 {
				fmt.Fprintf(os.Stderr, "Delete failed (attempt %d/%d): %s - %v\n", attempt+1, maxRetries, relPath, delErr)
				select {
				case <-time.After(delays[attempt]):
				case <-q.done:
					return
				}
			}
			continue
		}

		// Upload/UploadResume assume the remote directory already exists.
		// Once ensured, remember it: repeated saves into the same folder —
		// the daemon's common case — then skip the check's round trips.
		remoteDir := path.Dir(remotePath)
		if q.dirKnown(task.profileName, remoteDir) {
			// already ensured
		} else if mkErr := client.MkdirAll(remoteDir); mkErr != nil {
			lastErr = mkErr
			if attempt < maxRetries-1 {
				fmt.Fprintf(os.Stderr, "Directory creation failed (attempt %d/%d): %s - %v\n", attempt+1, maxRetries, relPath, mkErr)
				select {
				case <-time.After(delays[attempt]):
				case <-q.done:
					return
				}
			}
			continue
		}

		q.rememberDir(task.profileName, remoteDir)

		// Use resume for large files (mirrors mirror.go behaviour)
		var uploadErr error
		if statErr == nil && fi.Size() >= transfer.ResumeThreshold {
			uploadErr = client.UploadResume(absFile, remotePath)
		} else {
			uploadErr = client.Upload(absFile, remotePath)
		}
		if uploadErr == nil {
			onSuccess(task.profileName, relPath)
			return
		}

		lastErr = uploadErr
		// The directory may have been removed remotely since it was
		// remembered: re-check it on the next attempt.
		q.forgetDirs(task.profileName)

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

// deleteRemote removes the remote counterpart of a locally deleted path,
// reporting whether anything was deleted. Already absent is not an error.
// A directory is only removed if it's empty — never recursively — so remote
// files that are .syncignore'd or were never synced are left alone; a
// non-empty directory is kept with a log line rather than retried.
func deleteRemote(client transfer.RemoteClient, remotePath, relPath string) (bool, error) {
	info, err := client.Stat(remotePath)
	if transfer.IsNotExist(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}

	if info.IsDir {
		if err := client.RemoveDir(remotePath); err != nil {
			fmt.Fprintf(os.Stderr, "Kept remote directory %s/ (not empty or not removable: %v)\n", relPath, err)
			return false, nil
		}
		fmt.Fprintf(os.Stderr, "Deleted remote directory: %s/\n", relPath)
		return true, nil
	}

	if err := client.Remove(remotePath); err != nil {
		if transfer.IsNotExist(err) {
			return false, nil
		}
		return false, err
	}
	fmt.Fprintf(os.Stderr, "Deleted remote: %s\n", relPath)
	return true, nil
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
	q.profiles = profiles
	q.profilesMu.Unlock()
	q.forgetDirs("") // remotePath or server may have changed
}

// dirKnown reports whether remoteDir was already ensured for profile.
func (q *UploadQueue) dirKnown(profile, remoteDir string) bool {
	q.dirsMu.Lock()
	defer q.dirsMu.Unlock()
	return q.knownDirs[profile+"\x00"+remoteDir]
}

// rememberDir records that remoteDir exists for profile.
func (q *UploadQueue) rememberDir(profile, remoteDir string) {
	q.dirsMu.Lock()
	defer q.dirsMu.Unlock()
	if q.knownDirs == nil {
		q.knownDirs = make(map[string]bool)
	}
	q.knownDirs[profile+"\x00"+remoteDir] = true
}

// forgetDirs drops remembered directories for profile ("" = all profiles).
func (q *UploadQueue) forgetDirs(profile string) {
	q.dirsMu.Lock()
	defer q.dirsMu.Unlock()
	for k := range q.knownDirs {
		if profile == "" || strings.HasPrefix(k, profile+"\x00") {
			delete(q.knownDirs, k)
		}
	}
}
