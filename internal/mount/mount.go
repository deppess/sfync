package mount

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/deppess/sfync/internal/config"
)

const (
	MountBaseDir = ".mounted"
)

// isManagedMountPoint reports whether mountPoint is a directory that
// sfync itself created (i.e. under ~/.mounted/). User-specified context
// directories must never be deleted by unmount operations.
func isManagedMountPoint(mountPoint string) bool {
	home, err := os.UserHomeDir()
	if err != nil {
		return false
	}
	base := filepath.Join(home, MountBaseDir) + string(filepath.Separator)
	return strings.HasPrefix(mountPoint, base)
}

// GetMountPoint returns the mount point path for a profile
// If profile has a context set, use that; otherwise use ~/.mounted/<profileName>
func GetMountPoint(profileName string, profile *config.Profile) (string, error) {
	// If context is set in config, use it
	if profile != nil && profile.Context != "" {
		return profile.Context, nil
	}

	// Otherwise use default: ~/.mounted/<profileName>
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("cannot determine home directory: %w", err)
	}
	return filepath.Join(home, MountBaseDir, profileName), nil
}

// IsMounted checks if a profile is currently mounted
func IsMounted(profileName string, profile *config.Profile) bool {
	mountPoint, err := GetMountPoint(profileName, profile)
	if err != nil {
		return false
	}

	// Check using mountpoint command
	cmd := exec.Command("mountpoint", "-q", mountPoint)
	err = cmd.Run()
	return err == nil
}

// ListMounted returns a list of currently mounted profiles
func ListMounted() ([]string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, fmt.Errorf("cannot determine home directory: %w", err)
	}
	baseDir := filepath.Join(home, MountBaseDir)

	// Check if base directory exists
	if _, err := os.Stat(baseDir); errors.Is(err, fs.ErrNotExist) {
		return []string{}, nil
	}

	// Read directory
	entries, err := os.ReadDir(baseDir)
	if err != nil {
		return nil, err
	}

	var mounted []string
	for _, entry := range entries {
		if entry.IsDir() {
			profileName := entry.Name()
			// Check default mount point (nil profile)
			if IsMounted(profileName, nil) {
				mounted = append(mounted, profileName)
			}
		}
	}

	return mounted, nil
}

// Mount mounts a remote filesystem based on the protocol
func Mount(profileName string, profile *config.Profile) error {
	mountPoint, err := GetMountPoint(profileName, profile)
	if err != nil {
		return err
	}

	// Check if already mounted
	if IsMounted(profileName, profile) {
		return fmt.Errorf("profile '%s' is already mounted at %s", profileName, mountPoint)
	}

	// Check if remote is reachable
	if err := IsReachable(profile); err != nil {
		return fmt.Errorf("remote unreachable: %w", err)
	}

	// Create mount point directory
	if err = os.MkdirAll(mountPoint, 0755); err != nil {
		return fmt.Errorf("failed to create mount point: %w", err)
	}

	// Mount based on protocol
	if profile.Protocol == "sftp" {
		err = mountSSHFS(profile, mountPoint)
	} else {
		err = mountRclone(profile, mountPoint)
	}

	if err != nil {
		// Clean up mount point on failure — only if sfync created it
		if isManagedMountPoint(mountPoint) {
			os.RemoveAll(mountPoint)
		}
		return err
	}

	// Verify mount succeeded
	if !IsMounted(profileName, profile) {
		if isManagedMountPoint(mountPoint) {
			os.RemoveAll(mountPoint)
		}
		return fmt.Errorf("mount verification failed")
	}

	return nil
}

// Unmount unmounts a profile's filesystem
func Unmount(profileName string, profile *config.Profile) error {
	mountPoint, err := GetMountPoint(profileName, profile)
	if err != nil {
		return err
	}

	// Check if mounted
	if !IsMounted(profileName, profile) {
		return fmt.Errorf("profile '%s' is not mounted", profileName)
	}

	// Force unmount using fusermount
	cmd := exec.Command("fusermount", "-uz", mountPoint)
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("unmount failed: %w", err)
	}

	// Only remove the directory if sfync created it (~/.mounted/<name>).
	// Never delete a user-specified context directory.
	if isManagedMountPoint(mountPoint) {
		if err := os.RemoveAll(mountPoint); err != nil {
			return fmt.Errorf("failed to remove mount point: %w", err)
		}
	}

	return nil
}

// UnmountAll unmounts all currently mounted profiles
func UnmountAll() error {
	mounted, err := ListMounted()
	if err != nil {
		return err
	}

	var errors []string
	for _, profileName := range mounted {
		// Try to unmount with nil profile (uses default path)
		if err := Unmount(profileName, nil); err != nil {
			errors = append(errors, fmt.Sprintf("%s: %v", profileName, err))
		}
	}

	if len(errors) > 0 {
		return fmt.Errorf("errors unmounting: %s", strings.Join(errors, "; "))
	}

	return nil
}
