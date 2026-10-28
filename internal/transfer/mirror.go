package transfer

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/deppess/sfync/internal/config"
	"github.com/deppess/sfync/internal/syncignore"
)

// localEntry holds info about a local file during mirror
type localEntry struct {
	info os.FileInfo
}

// MirrorUp syncs local → remote (upload)
func MirrorUp(client RemoteClient, profile *config.Profile, dryRun bool) (*MirrorResult, error) {
	result := &MirrorResult{}

	// Ensure remote path exists (first sync scenario)
	if _, err := client.Stat(profile.RemotePath); err != nil {
		fmt.Fprintf(os.Stderr, "Remote path %s does not exist, creating...\n", profile.RemotePath)
		if mkErr := client.MkdirAll(profile.RemotePath); mkErr != nil {
			return nil, wrapError("mirror",
				fmt.Errorf("remote path %s does not exist and cannot be created: %w",
					profile.RemotePath, mkErr))
		}
	}

	// Load .syncignore
	patterns, err := syncignore.Load(profile.Context)
	if err != nil {
		return nil, fmt.Errorf("failed to load .syncignore: %w", err)
	}

	// Walk local
	fmt.Fprintf(os.Stderr, "Scanning local: %s\n", profile.Context)
	local, err := walkLocal(profile.Context, patterns)
	if err != nil {
		return nil, wrapError("scan local", err)
	}

	// Walk remote
	fmt.Fprintf(os.Stderr, "Scanning remote: %s\n", profile.RemotePath)
	remote, err := walkRemote(client, profile.RemotePath, patterns)
	if err != nil {
		return nil, wrapError("scan remote", err)
	}

	// Diff
	actions := diffTrees(local, remote, true)
	actions = sortActions(actions)

	// Count skips
	for _, a := range actions {
		if a.Action == ActionSkip {
			result.Skipped++
		}
	}

	// Dry run — return actions without executing
	if dryRun {
		result.Actions = actions
		result.Success = true
		return result, nil
	}

	// Execute
	executeUp(client, profile, actions, result)

	result.Success = len(result.Errors) == 0
	return result, nil
}

// MirrorDown syncs remote → local (download)
func MirrorDown(client RemoteClient, profile *config.Profile, dryRun bool) (*MirrorResult, error) {
	result := &MirrorResult{}

	// Remote path must exist for download
	if _, err := client.Stat(profile.RemotePath); err != nil {
		return nil, wrapError("mirror",
			fmt.Errorf("remote path does not exist: %s", profile.RemotePath))
	}

	// Load .syncignore
	patterns, err := syncignore.Load(profile.Context)
	if err != nil {
		return nil, fmt.Errorf("failed to load .syncignore: %w", err)
	}

	// Walk local
	fmt.Fprintf(os.Stderr, "Scanning local: %s\n", profile.Context)
	local, err := walkLocal(profile.Context, patterns)
	if err != nil {
		return nil, wrapError("scan local", err)
	}

	// Walk remote
	fmt.Fprintf(os.Stderr, "Scanning remote: %s\n", profile.RemotePath)
	remote, err := walkRemote(client, profile.RemotePath, patterns)
	if err != nil {
		return nil, wrapError("scan remote", err)
	}

	// Diff
	actions := diffTrees(local, remote, false)
	actions = sortActions(actions)

	// Count skips
	for _, a := range actions {
		if a.Action == ActionSkip {
			result.Skipped++
		}
	}

	// Dry run
	if dryRun {
		result.Actions = actions
		result.Success = true
		return result, nil
	}

	// Execute
	executeDown(client, profile, actions, result)

	result.Success = len(result.Errors) == 0
	return result, nil
}

// walkLocal walks the local directory tree, applying syncignore
func walkLocal(localRoot string, patterns []string) (map[string]localEntry, error) {
	entries := make(map[string]localEntry)

	err := filepath.WalkDir(localRoot, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			fmt.Fprintf(os.Stderr, "  warning: cannot access %s: %v\n", p, err)
			if d != nil && d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}

		// Skip symlinks
		if d.Type()&os.ModeSymlink != 0 {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}

		// Skip sfync's own in-progress transfer files (see partialSuffix)
		if !d.IsDir() && IsPartialName(d.Name()) {
			return nil
		}

		// Get relative path
		relPath, relErr := filepath.Rel(localRoot, p)
		if relErr != nil || relPath == "." {
			return nil
		}
		relPath = filepath.ToSlash(relPath)

		// Check syncignore
		if syncignore.ShouldIgnore(relPath, patterns) {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}

		// Get FileInfo — for files we need size+mtime; for dirs we need IsDir().
		// d.Info() is lazy on WalkDir (no extra syscall for dirs already stat'd).
		info, infoErr := d.Info()
		if infoErr != nil {
			fmt.Fprintf(os.Stderr, "  warning: cannot stat %s: %v\n", p, infoErr)
			return nil
		}
		entries[relPath] = localEntry{info: info}
		return nil
	})

	return entries, err
}

// walkRemote recursively lists the remote directory tree, applying syncignore.
//
// SFTP connections multiplex requests over a single SSH channel (the client
// is already configured with UseConcurrentReads/UseConcurrentWrites for
// transfers, so concurrent request dispatch is known-safe), so listing
// subdirectories in parallel is safe there and cuts scan time on
// latency-bound links. FTP's control connection allows only one command in
// flight at a time — issuing concurrent List calls on it would corrupt the
// session — so FTP always walks serially.
func walkRemote(client RemoteClient, remoteRoot string, patterns []string) (map[string]*RemoteFileInfo, error) {
	entries := make(map[string]*RemoteFileInfo)
	var mu sync.Mutex

	concurrent := supportsConcurrentRequests(client)
	var sem chan struct{}
	if concurrent {
		sem = make(chan struct{}, MaxConcurrentReadDir)
	}

	type subdir struct{ dir, rel string }

	var wg sync.WaitGroup
	var walk func(dir, relDir string)
	walk = func(dir, relDir string) {
		defer wg.Done()

		if concurrent {
			sem <- struct{}{}
			defer func() { <-sem }()
		}

		items, err := client.ReadDir(dir)
		if err != nil {
			fmt.Fprintf(os.Stderr, "  warning: cannot list %s: %v\n", dir, err)
			return // warn and skip, don't abort entire walk
		}

		var subdirs []subdir
		mu.Lock()
		for _, item := range items {
			// Skip sfync's own in-progress transfer files (see partialSuffix)
			if !item.IsDir && IsPartialName(item.Name) {
				continue
			}

			relPath := item.Name
			if relDir != "" {
				relPath = relDir + "/" + item.Name
			}

			// Check syncignore
			if syncignore.ShouldIgnore(relPath, patterns) {
				continue
			}

			info := item // copy
			info.Path = relPath
			entries[relPath] = &info

			if item.IsDir {
				subdirs = append(subdirs, subdir{dir + "/" + item.Name, relPath})
			}
		}
		mu.Unlock()

		for _, sd := range subdirs {
			wg.Add(1)
			if concurrent {
				go walk(sd.dir, sd.rel)
			} else {
				walk(sd.dir, sd.rel)
			}
		}
	}

	wg.Add(1)
	walk(remoteRoot, "")
	wg.Wait()

	return entries, nil
}

// diffTrees compares local and remote trees, producing an action list.
// upload=true means local→remote; upload=false means remote→local.
func diffTrees(local map[string]localEntry, remote map[string]*RemoteFileInfo, upload bool) []MirrorEntry {
	var actions []MirrorEntry

	if upload {
		// Local → remote

		// Files/dirs in local
		for relPath, entry := range local {
			remoteEntry, existsRemote := remote[relPath]

			if entry.info.IsDir() {
				if !existsRemote {
					actions = append(actions, MirrorEntry{Action: ActionMkdir, RelPath: relPath})
				} else if !remoteEntry.IsDir {
					actions = append(actions, MirrorEntry{Action: ActionConflict, RelPath: relPath})
				}
				continue
			}

			if !existsRemote {
				actions = append(actions, MirrorEntry{
					Action:    ActionUpload,
					RelPath:   relPath,
					LocalInfo: entry.info,
					IsNew:     true,
				})
			} else if remoteEntry.IsDir {
				actions = append(actions, MirrorEntry{Action: ActionConflict, RelPath: relPath})
			} else if isDifferent(entry.info, remoteEntry) {
				actions = append(actions, MirrorEntry{
					Action:     ActionUpload,
					RelPath:    relPath,
					LocalInfo:  entry.info,
					RemoteInfo: remoteEntry,
					IsNew:      false,
				})
			} else {
				actions = append(actions, MirrorEntry{Action: ActionSkip, RelPath: relPath})
			}
		}

		// Files/dirs in remote but NOT in local → delete
		for relPath, remoteEntry := range remote {
			if _, existsLocal := local[relPath]; !existsLocal {
				if remoteEntry.IsDir {
					actions = append(actions, MirrorEntry{Action: ActionDeleteDir, RelPath: relPath})
				} else {
					actions = append(actions, MirrorEntry{Action: ActionDelete, RelPath: relPath})
				}
			}
		}
	} else {
		// Remote → local (down)

		// Files/dirs in remote
		for relPath, remoteEntry := range remote {
			localEntry, existsLocal := local[relPath]

			if remoteEntry.IsDir {
				if !existsLocal {
					actions = append(actions, MirrorEntry{Action: ActionMkdir, RelPath: relPath})
				} else if !localEntry.info.IsDir() {
					actions = append(actions, MirrorEntry{Action: ActionConflict, RelPath: relPath})
				}
				continue
			}

			if !existsLocal {
				actions = append(actions, MirrorEntry{
					Action:     ActionDownload,
					RelPath:    relPath,
					RemoteInfo: remoteEntry,
					IsNew:      true,
				})
			} else if localEntry.info.IsDir() {
				actions = append(actions, MirrorEntry{Action: ActionConflict, RelPath: relPath})
			} else if isDifferent(localEntry.info, remoteEntry) {
				actions = append(actions, MirrorEntry{
					Action:     ActionDownload,
					RelPath:    relPath,
					LocalInfo:  localEntry.info,
					RemoteInfo: remoteEntry,
					IsNew:      false,
				})
			} else {
				actions = append(actions, MirrorEntry{Action: ActionSkip, RelPath: relPath})
			}
		}

		// Files/dirs in local but NOT in remote → delete local
		for relPath, localEntry := range local {
			if _, existsRemote := remote[relPath]; !existsRemote {
				if localEntry.info.IsDir() {
					actions = append(actions, MirrorEntry{Action: ActionDeleteDir, RelPath: relPath})
				} else {
					actions = append(actions, MirrorEntry{Action: ActionDelete, RelPath: relPath})
				}
			}
		}
	}

	return actions
}

// isDifferent compares a local file and remote file
func isDifferent(local os.FileInfo, remote *RemoteFileInfo) bool {
	// Size differs → definitely different
	if local.Size() != remote.Size {
		return true
	}
	// The remote time is truncated to its precision, so the true remote
	// mtime lies in [ModTime, ModTime+precision). In sync if the local mtime
	// falls in that window, plus 2 seconds of slack either side for
	// filesystem rounding (FAT-style 2s granularity, sub-second truncation).
	const slack = 2 * time.Second
	diff := local.ModTime().Sub(remote.ModTime)
	return diff < -slack || diff > remote.ModTimePrecision+slack
}

// sortActions orders actions for safe execution:
// 1. Mkdir (shallowest first — parents before children)
// 2. Upload/Download (any order)
// 3. Delete files (any order)
// 4. DeleteDir (deepest first — children before parents)
func sortActions(actions []MirrorEntry) []MirrorEntry {
	var mkdirs, transfers, deletes, deleteDirs, conflicts []MirrorEntry

	for _, a := range actions {
		switch a.Action {
		case ActionMkdir:
			mkdirs = append(mkdirs, a)
		case ActionUpload, ActionDownload:
			transfers = append(transfers, a)
		case ActionDelete:
			deletes = append(deletes, a)
		case ActionDeleteDir:
			deleteDirs = append(deleteDirs, a)
		case ActionConflict:
			conflicts = append(conflicts, a)
			// ActionSkip is not included in output
		}
	}

	// Mkdir: shallowest first
	sort.Slice(mkdirs, func(i, j int) bool {
		return strings.Count(mkdirs[i].RelPath, "/") < strings.Count(mkdirs[j].RelPath, "/")
	})

	// DeleteDir: deepest first
	sort.Slice(deleteDirs, func(i, j int) bool {
		return strings.Count(deleteDirs[i].RelPath, "/") > strings.Count(deleteDirs[j].RelPath, "/")
	})

	var result []MirrorEntry
	result = append(result, mkdirs...)
	result = append(result, transfers...)
	result = append(result, deletes...)
	result = append(result, deleteDirs...)
	result = append(result, conflicts...)
	return result
}

// executeUp applies mirror-up actions (local → remote) in four phases:
// mkdirs, uploads, file deletes, directory deletes. Each phase goes through
// runTransfers, so it runs in parallel when the client supports it, with
// ordering constraints kept where they matter: directories are created one
// depth level at a time (parents strictly before children) and removed
// deepest level first.
func executeUp(client RemoteClient, profile *config.Profile, actions []MirrorEntry, result *MirrorResult) {
	var mkdirs, uploads, deletes, deleteDirs []MirrorEntry
	for _, action := range actions {
		switch action.Action {
		case ActionMkdir:
			mkdirs = append(mkdirs, action)
		case ActionUpload:
			uploads = append(uploads, action)
		case ActionDelete:
			deletes = append(deletes, action)
		case ActionDeleteDir:
			deleteDirs = append(deleteDirs, action)
		case ActionConflict:
			result.Warnings = append(result.Warnings,
				fmt.Sprintf("skipped %s: file on one side, directory on the other — resolve manually", action.RelPath))
			fmt.Fprintf(os.Stderr, "  ! conflict (file vs directory), skipping: %s\n", action.RelPath)
		}
	}

	remote := func(a MirrorEntry) string { return RemotePath(profile.RemotePath, a.RelPath) }

	// 1. Mkdir — mkdirs arrive sorted shallowest first (sortActions), and the
	// remote root was ensured by MirrorUp, so each directory's parent exists
	// by the time its level runs: a single Mkdir round trip suffices instead
	// of MkdirAll's stat of every path component. MkdirAll remains the
	// fallback (already exists, or a parent unexpectedly missing).
	for _, level := range depthLevels(mkdirs) {
		runTransfers(client, jobsFor(level, func(a MirrorEntry) error {
			if err := client.Mkdir(remote(a)); err != nil {
				return client.MkdirAll(remote(a))
			}
			return nil
		}), func(a MirrorEntry, err error) {
			if err != nil {
				result.Warnings = append(result.Warnings,
					fmt.Sprintf("mkdir %s: %s", a.RelPath, translateError(err)))
				return
			}
			result.Created++
			fmt.Fprintf(os.Stderr, "  + mkdir: %s\n", a.RelPath)
		})
	}

	// 2. Uploads
	runTransfers(client, jobsFor(uploads, func(a MirrorEntry) error {
		return doUpload(client, filepath.Join(profile.Context, a.RelPath), remote(a), a)
	}), func(a MirrorEntry, err error) {
		if err != nil {
			result.Errors = append(result.Errors,
				fmt.Sprintf("upload %s: %s", a.RelPath, translateError(err)))
		} else {
			result.Uploaded++
		}
	})

	// 3. File deletes — independent of each other, fully parallel. A file
	// already gone (e.g. removed by the first try before a reconnect-retry)
	// counts as deleted.
	runTransfers(client, jobsFor(deletes, func(a MirrorEntry) error {
		if err := client.Remove(remote(a)); err != nil && !IsNotExist(err) {
			return err
		}
		return nil
	}), func(a MirrorEntry, err error) {
		if err != nil {
			result.Warnings = append(result.Warnings,
				fmt.Sprintf("delete %s: %s", a.RelPath, translateError(err)))
			return
		}
		result.Deleted++
		fmt.Fprintf(os.Stderr, "  - delete: %s\n", a.RelPath)
	})

	// 4. Directory deletes — deleteDirs arrive sorted deepest first, so each
	// level's children are gone before their parents are attempted.
	for _, level := range depthLevels(deleteDirs) {
		runTransfers(client, jobsFor(level, func(a MirrorEntry) error {
			return client.RemoveDir(remote(a))
		}), func(a MirrorEntry, err error) {
			if err != nil {
				result.Warnings = append(result.Warnings,
					fmt.Sprintf("rmdir %s: %s", a.RelPath, translateError(err)))
				return
			}
			result.Deleted++
			fmt.Fprintf(os.Stderr, "  - rmdir: %s/\n", a.RelPath)
		})
	}
}

// depthLevels splits depth-sorted entries into runs of equal depth (number
// of path separators), preserving order.
func depthLevels(entries []MirrorEntry) [][]MirrorEntry {
	var levels [][]MirrorEntry
	for i := 0; i < len(entries); {
		depth := strings.Count(entries[i].RelPath, "/")
		j := i
		for j < len(entries) && strings.Count(entries[j].RelPath, "/") == depth {
			j++
		}
		levels = append(levels, entries[i:j])
		i = j
	}
	return levels
}

// jobsFor wraps each entry in a transferJob running fn on it.
func jobsFor(entries []MirrorEntry, fn func(MirrorEntry) error) []transferJob {
	jobs := make([]transferJob, len(entries))
	for i, a := range entries {
		jobs[i] = transferJob{action: a, run: func() error { return fn(a) }}
	}
	return jobs
}

// executeDown applies mirror-down actions (remote → local). See executeUp
// for the mkdir/transfer split rationale.
func executeDown(client RemoteClient, profile *config.Profile, actions []MirrorEntry, result *MirrorResult) {
	var transferActions []MirrorEntry

	for _, action := range actions {
		switch action.Action {
		case ActionSkip:
			continue

		case ActionMkdir:
			localDir := filepath.Join(profile.Context, action.RelPath)
			if err := os.MkdirAll(localDir, 0755); err != nil {
				result.Warnings = append(result.Warnings,
					fmt.Sprintf("mkdir %s: %v", action.RelPath, err))
			} else {
				result.Created++
				fmt.Fprintf(os.Stderr, "  + mkdir: %s\n", action.RelPath)
			}

		case ActionDownload:
			transferActions = append(transferActions, action)

		case ActionDelete:
			localPath := filepath.Join(profile.Context, action.RelPath)
			if err := os.Remove(localPath); err != nil {
				result.Warnings = append(result.Warnings,
					fmt.Sprintf("delete %s: %v", action.RelPath, err))
			} else {
				result.Deleted++
				fmt.Fprintf(os.Stderr, "  - delete: %s\n", action.RelPath)
			}

		case ActionDeleteDir:
			localPath := filepath.Join(profile.Context, action.RelPath)
			// os.Remove only removes empty directories — protects local-only files
			// that were never synced from silent deletion.
			if err := os.Remove(localPath); err != nil {
				result.Warnings = append(result.Warnings,
					fmt.Sprintf("rmdir %s: %v", action.RelPath, err))
			} else {
				result.Deleted++
				fmt.Fprintf(os.Stderr, "  - rmdir: %s/\n", action.RelPath)
			}

		case ActionConflict:
			result.Warnings = append(result.Warnings,
				fmt.Sprintf("skipped %s: file on one side, directory on the other — resolve manually", action.RelPath))
			fmt.Fprintf(os.Stderr, "  ! conflict (file vs directory), skipping: %s\n", action.RelPath)
		}
	}

	jobs := make([]transferJob, len(transferActions))
	for i, action := range transferActions {
		rPath := RemotePath(profile.RemotePath, action.RelPath)
		localPath := filepath.Join(profile.Context, action.RelPath)
		jobs[i] = transferJob{
			action: action,
			run:    func() error { return client.Download(rPath, localPath) },
		}
	}

	runTransfers(client, jobs, func(action MirrorEntry, err error) {
		if err != nil {
			result.Errors = append(result.Errors,
				fmt.Sprintf("download %s: %s", action.RelPath, translateError(err)))
		} else {
			result.Downloaded++
		}
	})
}

// transferJob is one queued upload or download.
type transferJob struct {
	action MirrorEntry
	run    func() error
}

// reconnectCoordinator lets multiple goroutines sharing one RemoteClient
// coordinate reconnect-on-failure without corrupting the shared connection.
//
// RemoteClient implementations (sftpClient in particular) hold their
// underlying connection in plain, unsynchronized struct fields — Connect/
// Close reassign those fields, which is only safe if no other goroutine is
// concurrently using them. So a reconnect must never run while any other
// worker still has a request in flight on the same client, and no worker
// may start a new request while a reconnect is in progress.
//
// runGuarded takes rw for reading around each transfer attempt, so any
// number of workers can transfer concurrently; reconnect takes rw for
// writing, so it can only proceed once every in-flight transfer has
// finished, and blocks new transfers from starting until it's done. gen is
// layered on top purely to collapse duplicate reconnects: if several
// workers fail at once (the same underlying connection died under all of
// them), only the first to reach reconnect() actually closes+reconnects —
// the rest see the generation has already moved on and just retry.
type reconnectCoordinator struct {
	rw  sync.RWMutex
	gen int
}

// runGuarded runs fn (a single transfer attempt) holding a read lock, and
// returns the generation observed at the time — pass that to reconnect if
// fn fails.
func (rc *reconnectCoordinator) runGuarded(fn func() error) (error, int) {
	rc.rw.RLock()
	defer rc.rw.RUnlock()
	return fn(), rc.gen
}

// healthy reports whether a failure observed at observedGen happened on a
// connection that is still fine — i.e. no reconnect has happened since and
// the connection still answers. Such a failure is a genuine per-file error
// (permission denied, disk full) that a reconnect wouldn't fix; reconnecting
// anyway would stall every other worker for nothing. Holds the read lock so
// the liveness probe can't race a reconnect's Close/Connect.
func (rc *reconnectCoordinator) healthy(client RemoteClient, observedGen int) bool {
	rc.rw.RLock()
	defer rc.rw.RUnlock()
	return rc.gen == observedGen && client.IsAlive()
}

// reconnect reconnects client, unless another worker already reconnected
// since observedGen (in which case it's a no-op — the caller should just
// retry). Blocks until every in-flight runGuarded call has returned.
func (rc *reconnectCoordinator) reconnect(client RemoteClient, observedGen int) error {
	rc.rw.Lock()
	defer rc.rw.Unlock()
	if rc.gen != observedGen {
		return nil // another worker already reconnected since we failed
	}
	client.Close()
	if err := client.Connect(); err != nil {
		return err
	}
	rc.gen++
	return nil
}

// runTransfers executes transfer jobs, in parallel (bounded by
// MaxConcurrentTransfers) when the client supports concurrent requests —
// SFTP only, since pkg/sftp's Client is documented safe for concurrent use
// from multiple goroutines. FTP's single control connection allows only one
// command in flight at a time, so it always runs with a single worker, which
// reduces to the original one-at-a-time behavior. onResult runs under an
// internal lock, so it's safe for it to mutate shared aggregate state (e.g.
// a MirrorResult) directly without its own locking.
func runTransfers(client RemoteClient, jobs []transferJob, onResult func(action MirrorEntry, err error)) {
	if len(jobs) == 0 {
		return
	}

	concurrent := supportsConcurrentRequests(client)
	workers := 1
	if concurrent {
		workers = min(MaxConcurrentTransfers, len(jobs))
	}

	var rc reconnectCoordinator
	var resultMu sync.Mutex
	jobsCh := make(chan transferJob)
	var wg sync.WaitGroup

	for w := 0; w < workers; w++ {
		wg.Go(func() {
			for job := range jobsCh {
				err, gen := rc.runGuarded(job.run)
				if err != nil && !rc.healthy(client, gen) {
					// Connection died (or was already replaced by another
					// worker): reconnect if still needed, then retry once
					fmt.Fprintf(os.Stderr, "  retry: %s\n", job.action.RelPath)
					if rerr := rc.reconnect(client, gen); rerr == nil {
						err, _ = rc.runGuarded(job.run)
					}
				}

				resultMu.Lock()
				onResult(job.action, err)
				resultMu.Unlock()
			}
		})
	}

	for _, j := range jobs {
		jobsCh <- j
	}
	close(jobsCh)
	wg.Wait()
}

// doUpload performs a single upload, choosing resume for large files
func doUpload(client RemoteClient, localPath, remotePath string, action MirrorEntry) error {
	if action.LocalInfo != nil && action.LocalInfo.Size() >= ResumeThreshold {
		return client.UploadResume(localPath, remotePath)
	}
	return client.Upload(localPath, remotePath)
}
