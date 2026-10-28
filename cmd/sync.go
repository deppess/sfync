package cmd

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/deppess/sfync/internal/config"
	"github.com/deppess/sfync/internal/notify"
	"github.com/deppess/sfync/internal/transfer"
)

// findContext determines the project root for a profile operation.
// Priority: 1) config context, 2) .git walk-up from filePath, 3) cwd
func findContext(profile *config.Profile, filePath string) (string, error) {
	if profile.Context != "" {
		return profile.Context, nil
	}
	if filePath == "" || !filepath.IsAbs(filePath) {
		return os.Getwd()
	}
	dir := filepath.Dir(filePath)
	homeDir, _ := os.UserHomeDir()
	for {
		if _, err := os.Stat(filepath.Join(dir, ".git")); err == nil {
			return dir, nil
		}
		if dir == homeDir || dir == "/" {
			break
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	return "", fmt.Errorf("no project root found (no .git). Set 'context' in config for this profile")
}

// resolveProfile is the common setup shared by Up, Down, Diff, Push, and Pull:
// checks deps, loads config, resolves profile, and auto-detects context.
func resolveProfile(profileName, contextFile string) (*config.Profile, error) {
	if err := notify.EnsureReady(); err != nil {
		notify.Error("SFTP Sync Error", err.Error())
		return nil, err
	}

	cfg, err := config.Load()
	if err != nil {
		notify.Error("SFTP Sync Error", err.Error())
		return nil, err
	}

	profile, err := cfg.GetProfile(profileName)
	if err != nil {
		notify.Error("SFTP Sync Error", err.Error())
		return nil, err
	}

	contextDir, err := findContext(profile, contextFile)
	if err != nil {
		notify.Error("SFTP Sync Error", err.Error())
		return nil, err
	}
	if profile.Context == "" {
		profile.Context = contextDir
	}

	return profile, nil
}

// direction describes one way of mirroring for runMirror's messages.
type direction struct {
	verb  string // "Upload"
	doing string // "Uploading to"
	done  string // "Uploaded to"
	noun  string // "upload"
	run   func(transfer.RemoteClient, *config.Profile, bool) (*transfer.MirrorResult, error)
	moved func(*transfer.MirrorResult) int
}

var (
	upload = direction{
		verb: "Upload", doing: "Uploading to", done: "Uploaded to", noun: "upload",
		run:   transfer.MirrorUp,
		moved: func(r *transfer.MirrorResult) int { return r.Uploaded },
	}
	download = direction{
		verb: "Download", doing: "Downloading from", done: "Downloaded from", noun: "download",
		run:   transfer.MirrorDown,
		moved: func(r *transfer.MirrorResult) int { return r.Downloaded },
	}
)

// Up performs full upload sync
func Up(profileName, contextFile string) error {
	return runMirror(profileName, contextFile, upload)
}

// Down performs full download sync
func Down(profileName, contextFile string) error {
	return runMirror(profileName, contextFile, download)
}

// runMirror connects, mirrors in direction d, and reports the outcome on
// the terminal and as a desktop notification.
func runMirror(profileName, contextFile string, d direction) error {
	profile, err := resolveProfile(profileName, contextFile)
	if err != nil {
		return err
	}

	notify.Info("SFTP Sync", fmt.Sprintf("%s %s...", d.doing, profile.Host))

	client := transfer.NewClient(profile)
	if err := client.Connect(); err != nil {
		notify.Error("SFTP Error", fmt.Sprintf("Failed to connect: %s", err.Error()))
		return err
	}
	defer client.Close()

	result, err := d.run(client, profile, false)
	if err != nil {
		notify.Error("SFTP Error", err.Error())
		return err
	}

	// Warnings (failed deletes, chmod, conflicts) are shown whether or not
	// the sync succeeded overall — on failure they're often the context.
	for _, w := range result.Warnings {
		fmt.Fprintf(os.Stderr, "  warning: %s\n", w)
	}

	if result.Success {
		total := d.moved(result) + result.Deleted + result.Created
		if len(result.Warnings) > 0 {
			notify.Warning("SFTP Sync Complete",
				fmt.Sprintf("%s %s\n%s\n(%d warnings)", d.done, profile.Host, formatSummary(result), len(result.Warnings)))
			fmt.Printf("⚠ %s complete: %s (%d warnings)\n", d.verb, formatSummary(result), len(result.Warnings))
		} else {
			notify.Success("SFTP Sync Complete", fmt.Sprintf("%s %s\n%s", d.done, profile.Host, formatSummary(result)))
			fmt.Printf("✓ %s complete: %s\n", d.verb, formatSummary(result))
		}
		if total == 0 && result.Skipped > 0 {
			fmt.Printf("  (all %d files already in sync)\n", result.Skipped)
		}
		return nil
	}

	for _, e := range result.Errors {
		fmt.Fprintf(os.Stderr, "  error: %s\n", e)
	}
	notify.Error("SFTP Error", fmt.Sprintf("%s failed with %d errors", d.verb, len(result.Errors)))
	return fmt.Errorf("%s failed with %d errors", d.noun, len(result.Errors))
}

// Diff shows a dry-run preview of what would change.
// direction must be "up" (upload preview) or "down" (download preview).
func Diff(direction, profileName, contextFile string) error {
	if direction != "up" && direction != "down" {
		return fmt.Errorf("unknown diff direction %q — use 'up' or 'down'", direction)
	}

	profile, err := resolveProfile(profileName, contextFile)
	if err != nil {
		return err
	}

	client := transfer.NewClient(profile)
	if err := client.Connect(); err != nil {
		notify.Error("SFTP Error", fmt.Sprintf("Failed to connect: %s", err.Error()))
		return err
	}
	defer client.Close()

	var result *transfer.MirrorResult
	if direction == "up" {
		result, err = transfer.MirrorUp(client, profile, true)
	} else {
		result, err = transfer.MirrorDown(client, profile, true)
	}
	if err != nil {
		notify.Error("SFTP Error", "Diff failed")
		return err
	}

	changes := 0
	for _, entry := range result.Actions {
		switch entry.Action {
		case transfer.ActionUpload:
			size := ""
			if entry.LocalInfo != nil {
				size = fmt.Sprintf(" (%s)", transfer.HumanSize(entry.LocalInfo.Size()))
			}
			if entry.IsNew {
				fmt.Printf("  + %s%s\n", entry.RelPath, size)
			} else {
				fmt.Printf("  ~ %s%s\n", entry.RelPath, size)
			}
			changes++
		case transfer.ActionDownload:
			size := ""
			if entry.RemoteInfo != nil {
				size = fmt.Sprintf(" (%s)", transfer.HumanSize(entry.RemoteInfo.Size))
			}
			if entry.IsNew {
				fmt.Printf("  + %s%s\n", entry.RelPath, size)
			} else {
				fmt.Printf("  ~ %s%s\n", entry.RelPath, size)
			}
			changes++
		case transfer.ActionMkdir:
			fmt.Printf("  + %s/\n", entry.RelPath)
			changes++
		case transfer.ActionDelete:
			fmt.Printf("  - %s\n", entry.RelPath)
			changes++
		case transfer.ActionDeleteDir:
			fmt.Printf("  - %s/\n", entry.RelPath)
			changes++
		case transfer.ActionConflict:
			fmt.Printf("  ! %s (conflict: file vs directory — resolve manually)\n", entry.RelPath)
			changes++
		}
	}

	if changes == 0 {
		fmt.Println("No differences found — local and remote are in sync.")
	} else {
		verb := "uploaded"
		if direction == "down" {
			verb = "downloaded"
		}
		fmt.Printf("\n%d changes would be %s.\n", changes, verb)
	}

	return nil
}

// formatSummary builds a human-readable summary from MirrorResult
func formatSummary(r *transfer.MirrorResult) string {
	var parts []string
	if r.Uploaded > 0 {
		parts = append(parts, fmt.Sprintf("%d uploaded", r.Uploaded))
	}
	if r.Downloaded > 0 {
		parts = append(parts, fmt.Sprintf("%d downloaded", r.Downloaded))
	}
	if r.Created > 0 {
		parts = append(parts, fmt.Sprintf("%d dirs created", r.Created))
	}
	if r.Deleted > 0 {
		parts = append(parts, fmt.Sprintf("%d deleted", r.Deleted))
	}
	if len(parts) == 0 {
		return "0 changes"
	}
	return strings.Join(parts, ", ")
}
