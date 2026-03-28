package watcher

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/fsnotify/fsnotify"

	"github.com/deppess/sfync/internal/config"
	"github.com/deppess/sfync/internal/syncignore"
)

// Watcher watches file changes for auto-sync
type Watcher struct {
	fsWatcher     *fsnotify.Watcher
	debouncer     *Debouncer
	profiles      map[string]*config.Profile // profile name -> profile
	callbacks     map[string]func(string)    // profile name -> upload callback
	contextDepths map[string]int             // profile name -> path separator count (precomputed)
	// syncignore cache: context path -> parsed patterns.
	// Invalidated when fsnotify fires a Write/Create on the .syncignore file.
	syncignoreCache   map[string][]string
	syncignoreCacheMu sync.RWMutex
}

// New creates a new watcher
func New() (*Watcher, error) {
	fsWatcher, err := fsnotify.NewWatcher()
	if err != nil {
		return nil, fmt.Errorf("failed to create file watcher: %w", err)
	}

	return &Watcher{
		fsWatcher:       fsWatcher,
		debouncer:       NewDebouncer(),
		profiles:        make(map[string]*config.Profile),
		callbacks:       make(map[string]func(string)),
		contextDepths:   make(map[string]int),
		syncignoreCache: make(map[string][]string),
	}, nil
}

// getPatterns returns the .syncignore patterns for a context directory.
// Results are cached in memory and invalidated when the .syncignore file changes.
func (w *Watcher) getPatterns(contextPath string) []string {
	// Fast path: cache hit
	w.syncignoreCacheMu.RLock()
	if patterns, ok := w.syncignoreCache[contextPath]; ok {
		w.syncignoreCacheMu.RUnlock()
		return patterns
	}
	w.syncignoreCacheMu.RUnlock()

	// Slow path: load from disk and cache
	patterns, _ := syncignore.Load(contextPath)
	w.syncignoreCacheMu.Lock()
	w.syncignoreCache[contextPath] = patterns
	w.syncignoreCacheMu.Unlock()
	return patterns
}

// invalidatePatterns evicts the cached patterns for a context directory.
// Called when fsnotify detects a change to the .syncignore file.
func (w *Watcher) invalidatePatterns(contextPath string) {
	w.syncignoreCacheMu.Lock()
	delete(w.syncignoreCache, contextPath)
	w.syncignoreCacheMu.Unlock()
}

// Watch starts watching a profile's context directory
func (w *Watcher) Watch(profileName string, profile *config.Profile, callback func(filePath string)) error {
	// Validate context exists
	if _, err := os.Stat(profile.Context); errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("context directory doesn't exist: %s", profile.Context)
	}

	// Store profile, callback, and precomputed context depth
	w.profiles[profileName] = profile
	w.callbacks[profileName] = callback
	w.contextDepths[profileName] = strings.Count(profile.Context, string(os.PathSeparator))

	// Add context directory to watcher (recursively)
	if err := w.addRecursive(profile.Context); err != nil {
		return fmt.Errorf("failed to watch directory: %w", err)
	}

	fmt.Fprintf(os.Stderr, "Watching: %s (%s)\n", profileName, profile.Context)
	return nil
}

// addRecursive adds a directory and all its subdirectories to the watcher,
// skipping directories matched by .syncignore to avoid wasting inotify watches
func (w *Watcher) addRecursive(root string) error {
	patterns := w.getPatterns(root)

	return filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}

		// Skip symlinks
		if d.Type()&os.ModeSymlink != 0 {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}

		// Only watch directories
		if !d.IsDir() {
			return nil
		}

		// Check syncignore for directories (skip entire subtree)
		if relPath, relErr := filepath.Rel(root, p); relErr == nil && relPath != "." {
			if syncignore.ShouldIgnore(filepath.ToSlash(relPath), patterns) {
				return filepath.SkipDir
			}
		}

		if err := w.fsWatcher.Add(p); err != nil {
			return err
		}

		return nil
	})
}

// Unwatch stops watching a profile
func (w *Watcher) Unwatch(profileName string) error {
	profile, exists := w.profiles[profileName]
	if !exists {
		return fmt.Errorf("profile not watched: %s", profileName)
	}

	// Remove context directory from watcher
	if err := w.removeRecursive(profile.Context); err != nil {
		return err
	}

	// Remove from maps
	delete(w.profiles, profileName)
	delete(w.callbacks, profileName)
	delete(w.contextDepths, profileName)

	fmt.Fprintf(os.Stderr, "Stopped watching: %s\n", profileName)
	return nil
}

// removeRecursive removes a directory and all subdirectories from watcher
func (w *Watcher) removeRecursive(root string) error {
	return filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil // Continue even if file doesn't exist
		}

		if d.IsDir() {
			w.fsWatcher.Remove(path) //nolint:errcheck // best-effort removal
		}

		return nil
	})
}

// Start starts processing file system events
func (w *Watcher) Start() {
	go func() {
		for {
			select {
			case event, ok := <-w.fsWatcher.Events:
				if !ok {
					return
				}

				// Invalidate syncignore cache when a .syncignore file changes
				if filepath.Base(event.Name) == ".syncignore" {
					if event.Op&fsnotify.Write == fsnotify.Write || event.Op&fsnotify.Create == fsnotify.Create {
						contextPath := filepath.Dir(event.Name)
						w.invalidatePatterns(contextPath)
						fmt.Fprintf(os.Stderr, "Reloaded .syncignore: %s\n", event.Name)
					}
				}

				// Only process WRITE and CREATE events
				if event.Op&fsnotify.Write == fsnotify.Write || event.Op&fsnotify.Create == fsnotify.Create {
					w.handleEvent(event)
				}

			case err, ok := <-w.fsWatcher.Errors:
				if !ok {
					return
				}
				fmt.Fprintf(os.Stderr, "Watcher error: %v\n", err)
			}
		}
	}()
}

// handleEvent processes a file system event
func (w *Watcher) handleEvent(event fsnotify.Event) {
	filePath := event.Name

	// Check if file is a directory or symlink (ignore)
	info, err := os.Lstat(filePath)
	if err != nil {
		// File might have been deleted, ignore
		return
	}

	if info.IsDir() {
		// Directory created — watch it if at least one matching profile does not
		// ignore it. Per-profile syncignore filtering happens at upload time, so
		// we only skip watching when every matching profile explicitly ignores it.
		if event.Op&fsnotify.Create == fsnotify.Create {
			for _, profile := range w.profiles {
				rel, relErr := filepath.Rel(profile.Context, filePath)
				if relErr != nil || strings.HasPrefix(rel, "..") {
					continue // filePath is not under this profile's context
				}
				patterns := w.getPatterns(profile.Context)
				if !syncignore.ShouldIgnore(filepath.ToSlash(rel), patterns) {
					w.fsWatcher.Add(filePath) //nolint:errcheck // best-effort
					return
				}
			}
		}
		return
	}

	// Skip symlinks
	if info.Mode()&os.ModeSymlink != 0 {
		return
	}

	// Find which profile(s) this file belongs to
	matchedProfiles := w.findMatchingProfiles(filePath)

	for _, profileName := range matchedProfiles {
		profile := w.profiles[profileName]
		callback := w.callbacks[profileName]

		// Get debounce delay (default 2000ms set in config.SetDefaults)
		delay := time.Duration(profile.AutoSyncDebounce) * time.Millisecond

		// Debounce key: profileName + filePath
		debounceKey := profileName + ":" + filePath

		// Add debounced callback
		w.debouncer.Add(debounceKey, delay, func() {
			callback(filePath)
		})
	}
}

// findMatchingProfiles finds which profile(s) a file belongs to
// Returns profile names sorted by context specificity (most specific first)
func (w *Watcher) findMatchingProfiles(filePath string) []string {
	var matches []struct {
		name  string
		depth int
	}

	for name, profile := range w.profiles {
		// Check if file is under this profile's context
		if strings.HasPrefix(filePath, profile.Context+"/") || filePath == profile.Context {
			// Use precomputed depth — avoids recomputing on every file event
			depth := w.contextDepths[name]
			matches = append(matches, struct {
				name  string
				depth int
			}{name, depth})
		}
	}

	if len(matches) == 0 {
		return nil
	}

	// Sort by depth (most specific = deepest path = highest depth)
	// For overlapping contexts, we want most specific only
	maxDepth := -1
	for _, m := range matches {
		if m.depth > maxDepth {
			maxDepth = m.depth
		}
	}

	// Return only the most specific matches
	var result []string
	for _, m := range matches {
		if m.depth == maxDepth {
			result = append(result, m.name)
		}
	}

	return result
}

// Close stops the watcher and cleans up
func (w *Watcher) Close() error {
	w.debouncer.StopAll()
	return w.fsWatcher.Close()
}
