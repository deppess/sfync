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
	absFile, err := filepath.Abs(filePath)
	if err != nil {
		notify.Error("SFTP Error", fmt.Sprintf("cannot resolve path '%s': %s", filePath, err))
		return fmt.Errorf("cannot resolve path '%s': %w", filePath, err)
	}

	relPath, ok := contextRel(profile.Context, absFile)
	if !ok {
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
	if err := client.MkdirAll(path.Dir(remotePath)); err != nil {
		notify.Error("SFTP Error", fmt.Sprintf("Failed to create remote directory for %s", relPath))
		return err
	}
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
	var relPath string
	absFile, _ := filepath.Abs(filePath)

	if rel, ok := contextRel(profile.Context, absFile); ok {
		// Path is within the local context — use it directly.
		relPath = rel
	} else {
		// Path is outside the local context.
		// Try interpreting it as an absolute remote path under profile.RemotePath.
		remotePosix := path.Clean(filepath.ToSlash(filePath))
		remotePrefix := path.Clean(profile.RemotePath)
		if remotePrefix != "/" { // the root already ends in a slash
			remotePrefix += "/"
		}
		if after, ok := strings.CutPrefix(remotePosix, remotePrefix); ok && after != "" {
			relPath = after
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

// contextRel returns absPath relative to the context directory, reporting
// whether it lies strictly inside it. Paths are compared as given first,
// then with symlinks resolved, since one directory often has two spellings:
// on macOS /var is /private/var and the working directory comes back
// resolved, and projects often live under a symlinked folder. Only the
// parent of absPath is resolved — absPath itself may not exist yet (a pull
// target), and if it is a symlink its own name is the one to sync.
// filepath.Rel cleans both paths, so a trailing slash on the context is
// fine.
func contextRel(context, absPath string) (string, bool) {
	if rel, ok := relInside(context, absPath); ok {
		return rel, true
	}
	realContext, err := filepath.EvalSymlinks(context)
	if err != nil {
		return "", false
	}
	return relInside(realContext, filepath.Join(resolveExisting(filepath.Dir(absPath)), filepath.Base(absPath)))
}

// relInside is filepath.Rel(base, p) when p lies strictly inside base.
func relInside(base, p string) (string, bool) {
	rel, err := filepath.Rel(base, p)
	if err != nil || rel == "." || escapesBase(rel) {
		return "", false
	}
	return rel, true
}

// resolveExisting resolves symlinks in the longest existing prefix of p.
func resolveExisting(p string) string {
	if real, err := filepath.EvalSymlinks(p); err == nil {
		return real
	}
	parent := filepath.Dir(p)
	if parent == p {
		return p
	}
	return filepath.Join(resolveExisting(parent), filepath.Base(p))
}

// escapesBase reports whether rel, a path from filepath.Rel, points outside
// its base directory. Testing for a ".." prefix alone would also match
// legitimate names that merely start with two dots (e.g. "..env.local").
func escapesBase(rel string) bool {
	return rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator))
}
