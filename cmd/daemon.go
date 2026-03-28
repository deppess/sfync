package cmd

import (
	"fmt"
	"os"
	"os/signal"
	"sync"
	"syscall"

	"github.com/fsnotify/fsnotify"

	"github.com/deppess/sfync/internal/config"
	"github.com/deppess/sfync/internal/deps"
	"github.com/deppess/sfync/internal/transfer"
	"github.com/deppess/sfync/internal/watcher"
)

// Daemon runs the auto-sync daemon
func Daemon() error {
	// Check dependencies
	if err := deps.CheckRequired("notify-send"); err != nil {
		return err
	}

	// Load config
	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("failed to load config: %w", err)
	}

	// Create watcher
	w, err := watcher.New()
	if err != nil {
		return fmt.Errorf("failed to create watcher: %w", err)
	}
	defer w.Close()

	// Build profiles map for queue
	profiles := make(map[string]*config.Profile)
	for name, profile := range cfg.Profiles {
		p := profile
		profiles[name] = &p
	}

	// Create connection pool
	pool := transfer.NewConnectionPool(profiles)

	// Create upload queue
	queue := watcher.NewUploadQueue(profiles, pool)

	// Create notifier with batching
	notifier := watcher.NewNotifier()

	// Start queue processor
	queue.Start(
		// On success
		func(profileName, relPath string) {
			fmt.Fprintf(os.Stderr, "✓ Uploaded: %s → %s\n", relPath, profileName)
			notifier.ResetErrorCount(profileName)
			notifier.NotifySuccess(profileName, relPath)
		},
		// On error
		func(profileName, relPath string, err error, failCount int) {
			fmt.Fprintf(os.Stderr, "✗ Upload failed after %d attempts: %s → %s (%v)\n", failCount, relPath, profileName, err)
			notifier.NotifyError(profileName, relPath, err)
		},
	)

	// watching tracks which profiles currently have active filesystem watches.
	// It is separate from profiles so the queue owns its own copy.
	// watchingMu guards all reads and writes to watching from both the main
	// goroutine and the config-reload goroutine.
	watching := make(map[string]*config.Profile)
	var watchingMu sync.Mutex

	watchedCount := 0
	watchingMu.Lock()
	for name, p := range profiles {
		if !p.AutoSync {
			continue
		}

		// Validate context exists
		if p.Context == "" {
			fmt.Fprintf(os.Stderr, "Warning: Profile '%s' has autoSync enabled but no context set (skipping)\n", name)
			continue
		}

		// Watch this profile
		err := w.Watch(name, p, func(filePath string) {
			queue.Enqueue(name, filePath)
		})

		if err != nil {
			fmt.Fprintf(os.Stderr, "Warning: Failed to watch profile '%s': %v (skipping)\n", name, err)
			continue
		}

		watching[name] = p
		watchedCount++
	}
	watchingMu.Unlock()

	if watchedCount == 0 {
		return fmt.Errorf("no profiles with autoSync enabled found")
	}

	fmt.Fprintf(os.Stderr, "Daemon started, watching %d profile(s)\n", watchedCount)

	// Start processing events
	w.Start()

	// Watch config file for changes
	configWatcher, err := fsnotify.NewWatcher()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Warning: Failed to create config watcher: %v\n", err)
	} else {
		defer configWatcher.Close()

		// Get config file path
		configPath, _ := config.GetConfigPath()

		err = configWatcher.Add(configPath)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Warning: Failed to watch config file: %v\n", err)
		} else {
			// Handle config changes in background
			go func() {
				for {
					select {
					case event, ok := <-configWatcher.Events:
						if !ok {
							return
						}

						// Config file changed (handle both WRITE and CREATE for atomic replacements)
						if event.Op&fsnotify.Write == fsnotify.Write || event.Op&fsnotify.Create == fsnotify.Create {
							fmt.Fprintf(os.Stderr, "Config file changed, reloading...\n")
							handleConfigReload(w, watching, &watchingMu, queue, pool)
						}

						// Re-add watch if file was removed (atomic replacement)
						if event.Op&fsnotify.Remove == fsnotify.Remove {
							configWatcher.Add(configPath)
						}

					case err, ok := <-configWatcher.Errors:
						if !ok {
							return
						}
						fmt.Fprintf(os.Stderr, "Config watcher error: %v\n", err)
					}
				}
			}()
		}
	}

	// Wait for interrupt signal
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)
	<-sigChan

	fmt.Fprintf(os.Stderr, "\nDaemon stopping...\n")
	queue.Stop()
	pool.CloseAll()
	return nil
}

// handleConfigReload reloads config and adjusts watched profiles.
// watching maps profile name → profile for profiles with active fsnotify watches.
// watchingMu guards all accesses to the watching map.
func handleConfigReload(w *watcher.Watcher, watching map[string]*config.Profile, watchingMu *sync.Mutex, queue *watcher.UploadQueue, pool *transfer.ConnectionPool) {
	// Load new config (I/O outside the lock)
	cfg, err := config.Load()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error reloading config: %v\n", err)
		return
	}

	// Build new profiles map
	newProfiles := make(map[string]*config.Profile)
	for name, profile := range cfg.Profiles {
		p := profile
		newProfiles[name] = &p
	}

	// Update connection pool (has its own internal mutex)
	pool.UpdateProfiles(newProfiles)

	// Lock for all reads/writes to the watching map
	watchingMu.Lock()
	defer watchingMu.Unlock()

	// 1. Stop watching profiles that were removed or have autoSync disabled;
	//    restart those whose context path changed.
	for oldName, oldProfile := range watching {
		newProfile, exists := newProfiles[oldName]

		if !exists || !newProfile.AutoSync {
			err := w.Unwatch(oldName)
			if err != nil {
				fmt.Fprintf(os.Stderr, "Warning: Failed to unwatch profile '%s': %v\n", oldName, err)
			} else {
				fmt.Fprintf(os.Stderr, "Stopped watching: %s (removed or autoSync disabled)\n", oldName)
			}
			delete(watching, oldName)
		} else if newProfile.Context != oldProfile.Context {
			// Context changed — restart the watcher on the new path.
			err := w.Unwatch(oldName)
			if err == nil {
				err = w.Watch(oldName, newProfile, func(filePath string) {
					queue.Enqueue(oldName, filePath)
				})
				if err != nil {
					// Unwatch succeeded but Watch failed: remove from tracking so
					// the next reload can retry rather than getting stuck.
					fmt.Fprintf(os.Stderr, "Warning: Failed to restart watching '%s': %v\n", oldName, err)
					delete(watching, oldName)
				} else {
					fmt.Fprintf(os.Stderr, "Restarted watching: %s (context changed)\n", oldName)
					watching[oldName] = newProfile
				}
			}
		} else {
			// Profile unchanged or only connection details changed — update
			// our tracking reference so future reloads compare against the
			// latest values.
			watching[oldName] = newProfile
		}
	}

	// 2. Start watching new profiles with autoSync enabled.
	for newName, newProfile := range newProfiles {
		if !newProfile.AutoSync {
			continue
		}

		if _, alreadyWatching := watching[newName]; !alreadyWatching {
			if newProfile.Context == "" {
				fmt.Fprintf(os.Stderr, "Warning: Profile '%s' has autoSync enabled but no context set (skipping)\n", newName)
				continue
			}

			err := w.Watch(newName, newProfile, func(filePath string) {
				queue.Enqueue(newName, filePath)
			})

			if err != nil {
				fmt.Fprintf(os.Stderr, "Warning: Failed to watch new profile '%s': %v\n", newName, err)
			} else {
				fmt.Fprintf(os.Stderr, "Started watching: %s (autoSync enabled)\n", newName)
				watching[newName] = newProfile
			}
		}
	}

	// Update the queue's profile map atomically. All I/O is done above so
	// the lock inside UpdateProfiles is held only for the map swap.
	queue.UpdateProfiles(newProfiles)
}
