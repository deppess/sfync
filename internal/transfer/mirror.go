package transfer

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
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

// walkRemote recursively lists the remote directory tree, applying syncignore
func walkRemote(client RemoteClient, remoteRoot string, patterns []string) (map[string]*RemoteFileInfo, error) {
	entries := make(map[string]*RemoteFileInfo)

	var walk func(dir, relDir string) error
	walk = func(dir, relDir string) error {
		items, err := client.ReadDir(dir)
		if err != nil {
			fmt.Fprintf(os.Stderr, "  warning: cannot list %s: %v\n", dir, err)
			return nil // warn and skip, don't abort entire walk
		}

		for _, item := range items {
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

			// Recurse into directories
			if item.IsDir {
				if err := walk(dir+"/"+item.Name, relPath); err != nil {
					return err
				}
			}
		}
		return nil
	}

	return entries, walk(remoteRoot, "")
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
	// ModTime differs by more than 2 seconds → different
	// 2-second tolerance handles FTP timestamp precision and filesystem rounding
	diff := local.ModTime().Sub(remote.ModTime)
	if diff < 0 {
		diff = -diff
	}
	return diff > 2*time.Second
}

// sortActions orders actions for safe execution:
// 1. Mkdir (shallowest first — parents before children)
// 2. Upload/Download (any order)
// 3. Delete files (any order)
// 4. DeleteDir (deepest first — children before parents)
func sortActions(actions []MirrorEntry) []MirrorEntry {
	var mkdirs, transfers, deletes, deleteDirs []MirrorEntry

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
	return result
}

// executeUp applies mirror-up actions (local → remote)
func executeUp(client RemoteClient, profile *config.Profile, actions []MirrorEntry, result *MirrorResult) {
	for _, action := range actions {
		switch action.Action {
		case ActionSkip:
			continue

		case ActionMkdir:
			rPath := RemotePath(profile.RemotePath, action.RelPath)
			if err := client.MkdirAll(rPath); err != nil {
				result.Warnings = append(result.Warnings,
					fmt.Sprintf("mkdir %s: %s", action.RelPath, translateError(err)))
			} else {
				result.Created++
				fmt.Fprintf(os.Stderr, "  + mkdir: %s\n", action.RelPath)
			}

		case ActionUpload:
			localPath := filepath.Join(profile.Context, action.RelPath)
			rPath := RemotePath(profile.RemotePath, action.RelPath)

			uploadErr := doUpload(client, localPath, rPath, action)

			if uploadErr != nil {
				// Try reconnect + retry once
				fmt.Fprintf(os.Stderr, "  retry: %s\n", action.RelPath)
				client.Close()
				if reconnErr := client.Connect(); reconnErr == nil {
					uploadErr = doUpload(client, localPath, rPath, action)
				}
			}

			if uploadErr != nil {
				result.Errors = append(result.Errors,
					fmt.Sprintf("upload %s: %s", action.RelPath, translateError(uploadErr)))
			} else {
				result.Uploaded++
			}

		case ActionDelete:
			rPath := RemotePath(profile.RemotePath, action.RelPath)
			if err := client.Remove(rPath); err != nil {
				result.Warnings = append(result.Warnings,
					fmt.Sprintf("delete %s: %s", action.RelPath, translateError(err)))
			} else {
				result.Deleted++
				fmt.Fprintf(os.Stderr, "  - delete: %s\n", action.RelPath)
			}

		case ActionDeleteDir:
			rPath := RemotePath(profile.RemotePath, action.RelPath)
			if err := client.RemoveDir(rPath); err != nil {
				result.Warnings = append(result.Warnings,
					fmt.Sprintf("rmdir %s: %s", action.RelPath, translateError(err)))
			} else {
				result.Deleted++
				fmt.Fprintf(os.Stderr, "  - rmdir: %s/\n", action.RelPath)
			}
		}
	}
}

// executeDown applies mirror-down actions (remote → local)
func executeDown(client RemoteClient, profile *config.Profile, actions []MirrorEntry, result *MirrorResult) {
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
			rPath := RemotePath(profile.RemotePath, action.RelPath)
			localPath := filepath.Join(profile.Context, action.RelPath)

			downloadErr := client.Download(rPath, localPath)

			if downloadErr != nil {
				// Try reconnect + retry once
				fmt.Fprintf(os.Stderr, "  retry: %s\n", action.RelPath)
				client.Close()
				if reconnErr := client.Connect(); reconnErr == nil {
					downloadErr = client.Download(rPath, localPath)
				}
			}

			if downloadErr != nil {
				result.Errors = append(result.Errors,
					fmt.Sprintf("download %s: %s", action.RelPath, translateError(downloadErr)))
			} else {
				result.Downloaded++
			}

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
		}
	}
}

// doUpload performs a single upload, choosing resume for large files
func doUpload(client RemoteClient, localPath, remotePath string, action MirrorEntry) error {
	if action.LocalInfo != nil && action.LocalInfo.Size() >= ResumeThreshold {
		return client.UploadResume(localPath, remotePath)
	}
	return client.Upload(localPath, remotePath)
}
