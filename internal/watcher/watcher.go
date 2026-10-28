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
	"github.com/deppess/sfync/internal/transfer"
)

// Watcher watches file changes for auto-sync
type Watcher struct {
	fsWatcher *fsnotify.Watcher
	debouncer *Debouncer

	// profiles/callbacks/contextDepths are written by Watch/Unwatch (called
	// from the daemon's config-reload goroutine) and read by the fsnotify
	// event-processing goroutine started in Start() — both run concurrently
	// once the daemon is up, so all access must go through profilesMu.
	profiles      map[string]*config.Profile // profile name -> profile
	callbacks     map[string]func(string)    // profile name -> upload callback
	contextDepths map[string]int             // profile name -> path separator count (precomputed)
	profilesMu    sync.RWMutex

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
	// Work on a copy with the context path cleaned: fsnotify reports clean
	// paths, and every comparison here (which profile a file belongs to, the
	// .syncignore cache key) assumes the same form — a context configured
	// with a trailing slash would otherwise match no file at all.
	cleaned := *profile
	cleaned.Context = filepath.Clean(profile.Context)
	profile = &cleaned

	// Validate context exists
	if _, err := os.Stat(profile.Context); errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("context directory doesn't exist: %s", profile.Context)
	}

	// Store profile, callback, and precomputed context depth
	w.profilesMu.Lock()
	w.profiles[profileName] = profile
	w.callbacks[profileName] = callback
	w.contextDepths[profileName] = strings.Count(profile.Context, string(os.PathSeparator))
	w.profilesMu.Unlock()

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
	w.profilesMu.RLock()
	profile, exists := w.profiles[profileName]
	w.profilesMu.RUnlock()
	if !exists {
		return fmt.Errorf("profile not watched: %s", profileName)
	}

	// Remove context directory from watcher
	if err := w.removeRecursive(profile.Context); err != nil {
		return err
	}

	// Remove from maps
	w.profilesMu.Lock()
	delete(w.profiles, profileName)
	delete(w.callbacks, profileName)
	delete(w.contextDepths, profileName)
	w.profilesMu.Unlock()

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

				switch {
				case event.Op&fsnotify.Write == fsnotify.Write || event.Op&fsnotify.Create == fsnotify.Create:
					w.handleEvent(event)
				case event.Op&fsnotify.Remove == fsnotify.Remove || event.Op&fsnotify.Rename == fsnotify.Rename:
					// The path is gone (a rename reports the old name; the new
					// name arrives as its own Create). Schedule it anyway: the
					// queue re-checks when the debounce fires and either
					// uploads (the file came back, e.g. an editor's atomic
					// save) or, for profiles with autoSyncDeletes, deletes it
					// remotely. Deciding there rather than here also means a
					// config reload toggling autoSyncDeletes takes effect
					// immediately.
					if !transfer.IsPartialName(filepath.Base(event.Name)) {
						w.schedule(event.Name)
					}
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
		if event.Op&fsnotify.Create == fsnotify.Create {
			w.watchNewDir(filePath)
		}
		return
	}

	// Skip symlinks
	if info.Mode()&os.ModeSymlink != 0 {
		return
	}

	w.schedule(filePath)
}

// watchNewDir starts watching a newly created directory and everything
// already inside it.
//
// Directories and files can appear inside a new directory before the watch
// on it exists — `mkdir -p a/b`, a git checkout or pull, unpacking an
// archive, a build writing dist/js/... — and their creation events are
// then never delivered. So the new tree is walked: every subdirectory is
// watched, and every file found is scheduled (harmless if it was also seen
// by an event: the debouncer and queue collapse duplicates, and the queue
// applies .syncignore). Subtrees every matching profile ignores are
// skipped, so ignored directories still cost no watches.
func (w *Watcher) watchNewDir(dir string) {
	filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil // vanished meanwhile — nothing to watch
		}
		if d.Type()&os.ModeSymlink != 0 {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if d.IsDir() {
			if !w.watchedByAnyProfile(p) {
				return filepath.SkipDir
			}
			w.fsWatcher.Add(p) //nolint:errcheck // best-effort
			return nil
		}
		if d.Type().IsRegular() {
			w.schedule(p)
		}
		return nil
	})
}

// watchedByAnyProfile reports whether dir is inside some profile's context
// and not ignored by that profile's .syncignore. Per-profile filtering
// happens again at upload time, so a directory is watched unless every
// matching profile ignores it.
func (w *Watcher) watchedByAnyProfile(dir string) bool {
	w.profilesMu.RLock()
	defer w.profilesMu.RUnlock()
	for _, profile := range w.profiles {
		rel, relErr := filepath.Rel(profile.Context, dir)
		if relErr != nil || escapesBase(rel) {
			continue // not under this profile's context
		}
		if !syncignore.ShouldIgnore(filepath.ToSlash(rel), w.getPatterns(profile.Context)) {
			return true
		}
	}
	return false
}

// schedule debounces a sync of filePath for every profile it belongs to.
func (w *Watcher) schedule(filePath string) {
	// Find which profile(s) this file belongs to
	matchedProfiles := w.findMatchingProfiles(filePath)

	w.profilesMu.RLock()
	defer w.profilesMu.RUnlock()

	for _, profileName := range matchedProfiles {
		profile := w.profiles[profileName]
		callback := w.callbacks[profileName]
		if profile == nil || callback == nil {
			continue // unwatched by a config reload since findMatchingProfiles
		}

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
	w.profilesMu.RLock()
	defer w.profilesMu.RUnlock()

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

// escapesBase reports whether rel, a path from filepath.Rel, points outside
// its base directory. Testing for a ".." prefix alone would also match
// legitimate names that merely start with two dots (e.g. "..env.local").
func escapesBase(rel string) bool {
	return rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator))
}
