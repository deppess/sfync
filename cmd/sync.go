package cmd

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/deppess/sfync/internal/config"
	"github.com/deppess/sfync/internal/deps"
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
	if err := deps.CheckRequired("notify-send"); err != nil {
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

// Up performs full upload sync
func Up(profileName, contextFile string) error {
	profile, err := resolveProfile(profileName, contextFile)
	if err != nil {
		return err
	}

	notify.Info("SFTP Sync", fmt.Sprintf("Uploading to %s...", profile.Host))

	client := transfer.NewClient(profile)
	if err := client.Connect(); err != nil {
		notify.Error("SFTP Error", fmt.Sprintf("Failed to connect: %s", err.Error()))
		return err
	}
	defer client.Close()

	result, err := transfer.MirrorUp(client, profile, false)
	if err != nil {
		notify.Error("SFTP Error", err.Error())
		return err
	}

	if result.Success {
		total := result.Uploaded + result.Deleted + result.Created
		if len(result.Warnings) > 0 {
			for _, w := range result.Warnings {
				fmt.Fprintf(os.Stderr, "  warning: %s\n", w)
			}
			notify.Warning("SFTP Sync Complete",
				fmt.Sprintf("Uploaded to %s\n%s\n(%d warnings)", profile.Host, formatSummary(result), len(result.Warnings)))
			fmt.Printf("⚠ Upload complete: %s (%d warnings)\n", formatSummary(result), len(result.Warnings))
		} else {
			notify.Success("SFTP Sync Complete", fmt.Sprintf("Uploaded to %s\n%s", profile.Host, formatSummary(result)))
			fmt.Printf("✓ Upload complete: %s\n", formatSummary(result))
		}
		if total == 0 && result.Skipped > 0 {
			fmt.Printf("  (all %d files already in sync)\n", result.Skipped)
		}
		return nil
	}

	for _, e := range result.Errors {
		fmt.Fprintf(os.Stderr, "  error: %s\n", e)
	}
	notify.Error("SFTP Error", fmt.Sprintf("Upload failed with %d errors", len(result.Errors)))
	fmt.Fprintf(os.Stderr, "✗ Upload failed!\n")
	return fmt.Errorf("upload failed with %d errors", len(result.Errors))
}

// Down performs full download sync
func Down(profileName, contextFile string) error {
	profile, err := resolveProfile(profileName, contextFile)
	if err != nil {
		return err
	}

	notify.Info("SFTP Sync", fmt.Sprintf("Downloading from %s...", profile.Host))

	client := transfer.NewClient(profile)
	if err := client.Connect(); err != nil {
		notify.Error("SFTP Error", fmt.Sprintf("Failed to connect: %s", err.Error()))
		return err
	}
	defer client.Close()

	result, err := transfer.MirrorDown(client, profile, false)
	if err != nil {
		notify.Error("SFTP Error", err.Error())
		return err
	}

	if result.Success {
		total := result.Downloaded + result.Deleted + result.Created
		if len(result.Warnings) > 0 {
			for _, w := range result.Warnings {
				fmt.Fprintf(os.Stderr, "  warning: %s\n", w)
			}
			notify.Warning("SFTP Sync Complete",
				fmt.Sprintf("Downloaded from %s\n%s\n(%d warnings)", profile.Host, formatSummary(result), len(result.Warnings)))
			fmt.Printf("⚠ Download complete: %s (%d warnings)\n", formatSummary(result), len(result.Warnings))
		} else {
			notify.Success("SFTP Sync Complete", fmt.Sprintf("Downloaded from %s\n%s", profile.Host, formatSummary(result)))
			fmt.Printf("✓ Download complete: %s\n", formatSummary(result))
		}
		if total == 0 && result.Skipped > 0 {
			fmt.Printf("  (all %d files already in sync)\n", result.Skipped)
		}
		return nil
	}

	for _, e := range result.Errors {
		fmt.Fprintf(os.Stderr, "  error: %s\n", e)
	}
	notify.Error("SFTP Error", fmt.Sprintf("Download failed with %d errors", len(result.Errors)))
	fmt.Fprintf(os.Stderr, "✗ Download failed!\n")
	return fmt.Errorf("download failed with %d errors", len(result.Errors))
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
