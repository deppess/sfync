package cmd

import (
	"fmt"
	"path"
	"path/filepath"
	"strings"

	"github.com/deppess/sfync/internal/notify"
	"github.com/deppess/sfync/internal/syncignore"
	"github.com/deppess/sfync/internal/transfer"
)

// Push uploads a single file
func Push(profileName, filePath string) error {
	profile, err := resolveProfile(profileName, filePath)
	if err != nil {
		return err
	}

	// Resolve absolute path and compute relative path within context
	relPath := filepath.Base(filePath)
	absFile, err := filepath.Abs(filePath)
	if err == nil {
		if rel, err := filepath.Rel(profile.Context, absFile); err == nil {
			relPath = rel
		}
	}

	// Check file is within context
	if absFile != "" && !strings.HasPrefix(absFile, profile.Context+"/") && absFile != profile.Context {
		notify.Error("SFTP Error", fmt.Sprintf("file '%s' is not within context '%s'", absFile, profile.Context))
		return fmt.Errorf("file '%s' is not within context '%s'", absFile, profile.Context)
	}

	// Check .syncignore before connecting
	patterns, _ := syncignore.Load(profile.Context)
	if syncignore.ShouldIgnore(filepath.ToSlash(relPath), patterns) {
		return fmt.Errorf("file ignored by .syncignore: %s", relPath)
	}

	notify.Info("SFTP Sync", fmt.Sprintf("Uploading %s...", relPath))

	client := transfer.NewClient(profile)
	if err := client.Connect(); err != nil {
		notify.Error("SFTP Error", fmt.Sprintf("Failed to connect: %s", err.Error()))
		return err
	}
	defer client.Close()

	remotePath := path.Join(profile.RemotePath, filepath.ToSlash(relPath))
	if err := client.Upload(absFile, remotePath); err != nil {
		notify.Error("SFTP Error", fmt.Sprintf("Failed to upload %s", relPath))
		return err
	}

	notify.Success("File Uploaded", fmt.Sprintf("%s → %s", relPath, profile.Host))
	fmt.Printf("✓ Uploaded: %s\n", relPath)
	return nil
}

// Pull downloads a single file.
// filePath may be:
//   - A relative path within the local context (e.g. "public/index.html")
//   - An absolute remote path under profile.RemotePath (e.g. "/www/site/public/index.html")
//     which is auto-mapped to the corresponding local context location.
func Pull(profileName, filePath string) error {
	profile, err := resolveProfile(profileName, filePath)
	if err != nil {
		return err
	}

	// Resolve absolute local path and derive relPath within context.
	relPath := filepath.Base(filePath)
	absFile, _ := filepath.Abs(filePath)

	rel, relErr := filepath.Rel(profile.Context, absFile)
	if relErr == nil && !strings.HasPrefix(rel, "..") {
		// Path is within the local context — use it directly.
		relPath = rel
	} else {
		// Path is outside the local context.
		// Try interpreting it as an absolute remote path under profile.RemotePath.
		remotePosix := path.Clean(filepath.ToSlash(filePath))
		remoteBase := path.Clean(profile.RemotePath)
		if strings.HasPrefix(remotePosix, remoteBase+"/") {
			relPath = strings.TrimPrefix(remotePosix, remoteBase+"/")
			absFile = filepath.Join(profile.Context, filepath.FromSlash(relPath))
		} else {
			return fmt.Errorf("path '%s' is neither within local context '%s' nor under remote path '%s'",
				filePath, profile.Context, profile.RemotePath)
		}
	}

	// Check .syncignore before connecting
	patterns, _ := syncignore.Load(profile.Context)
	if syncignore.ShouldIgnore(filepath.ToSlash(relPath), patterns) {
		return fmt.Errorf("file ignored by .syncignore: %s", relPath)
	}

	notify.Info("SFTP Sync", fmt.Sprintf("Downloading %s...", relPath))

	client := transfer.NewClient(profile)
	if err := client.Connect(); err != nil {
		notify.Error("SFTP Error", fmt.Sprintf("Failed to connect: %s", err.Error()))
		return err
	}
	defer client.Close()

	remotePath := path.Join(profile.RemotePath, filepath.ToSlash(relPath))
	if err := client.Download(remotePath, absFile); err != nil {
		notify.Error("SFTP Error", fmt.Sprintf("Failed to download %s", relPath))
		return err
	}

	notify.Success("File Downloaded", fmt.Sprintf("%s ← %s", relPath, profile.Host))
	fmt.Printf("✓ Downloaded: %s\n", relPath)
	return nil
}

// Current uploads the current file (editor integration alias for Push)
func Current(profileName, filePath string) error {
	return Push(profileName, filePath)
}
