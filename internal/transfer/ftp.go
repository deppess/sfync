package transfer

import (
	"fmt"
	"io"
	"net"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/jlaffaye/ftp"

	"github.com/deppess/sfync/internal/config"
)

// ftpClient implements RemoteClient for FTP connections
type ftpClient struct {
	profile *config.Profile
	conn    *ftp.ServerConn
}

func newFTPClient(profile *config.Profile) *ftpClient {
	return &ftpClient{profile: profile}
}

// Connect establishes the FTP connection and logs in
func (c *ftpClient) Connect() error {
	// Safety: close existing connection if any
	if c.conn != nil {
		c.Close()
	}

	addr := net.JoinHostPort(c.profile.Host, strconv.Itoa(c.profile.Port))
	conn, err := ftp.Dial(addr, ftp.DialWithTimeout(DialTimeout))
	if err != nil {
		return wrapError("connect", err)
	}

	if err := conn.Login(c.profile.Username, c.profile.Password); err != nil {
		conn.Quit()
		return wrapError("connect", err)
	}

	c.conn = conn
	return nil
}

// Close disconnects. Safe to call multiple times.
func (c *ftpClient) Close() error {
	if c.conn != nil {
		err := c.conn.Quit()
		c.conn = nil
		return err
	}
	return nil
}

// IsAlive checks if the connection is still usable
func (c *ftpClient) IsAlive() bool {
	if c.conn == nil {
		return false
	}
	// NoOp might hang on a dead connection — race with timeout
	done := make(chan error, 1)
	go func() { done <- c.conn.NoOp() }()
	select {
	case err := <-done:
		return err == nil
	case <-time.After(AliveTimeout):
		return false
	}
}

// Stat returns info about a remote path
func (c *ftpClient) Stat(remotePath string) (*RemoteFileInfo, error) {
	entry, err := c.conn.GetEntry(remotePath)
	if err != nil {
		return nil, wrapError("stat", err)
	}
	return ftpEntryToRemote(entry, ""), nil
}

// ReadDir lists entries in a remote directory, skipping symlinks
func (c *ftpClient) ReadDir(remotePath string) ([]RemoteFileInfo, error) {
	entries, err := c.conn.List(remotePath)
	if err != nil {
		return nil, wrapError("readdir", err)
	}

	var result []RemoteFileInfo
	for _, entry := range entries {
		// Skip . and ..
		if entry.Name == "." || entry.Name == ".." {
			continue
		}
		// Skip symlinks
		if entry.Type == ftp.EntryTypeLink {
			continue
		}
		result = append(result, *ftpEntryToRemote(entry, ""))
	}
	return result, nil
}

// Upload streams a local file to the remote server
func (c *ftpClient) Upload(localPath, remotePath string) error {
	local, err := os.Open(localPath)
	if err != nil {
		return wrapError("upload", err)
	}
	defer local.Close()

	localInfo, err := local.Stat()
	if err != nil {
		return wrapError("upload", err)
	}

	// Ensure remote directory exists
	remoteDir := path.Dir(remotePath)
	if err := c.MkdirAll(remoteDir); err != nil {
		return wrapError("upload", fmt.Errorf("cannot create directory %s: %w", remoteDir, err))
	}

	// FTP Stor
	if err := c.conn.Stor(remotePath, local); err != nil {
		return wrapError("upload", err)
	}

	fmt.Fprintf(os.Stderr, "  uploaded: %s (%s)\n", remotePath, HumanSize(localInfo.Size()))
	return nil
}

// UploadResume uploads a file with resume support
func (c *ftpClient) UploadResume(localPath, remotePath string) error {
	localInfo, err := os.Stat(localPath)
	if err != nil {
		return wrapError("resume", err)
	}
	localSize := localInfo.Size()

	// Get remote size
	remoteSize, err := c.conn.FileSize(remotePath)
	if err != nil {
		// File doesn't exist remotely — full upload
		return c.Upload(localPath, remotePath)
	}

	if remoteSize == localSize {
		fmt.Fprintf(os.Stderr, "  skipped (already complete): %s\n", remotePath)
		return nil
	}
	if remoteSize > localSize {
		fmt.Fprintf(os.Stderr, "  re-uploading (remote larger than local): %s\n", remotePath)
		return c.Upload(localPath, remotePath)
	}

	// Resume: remote is smaller
	fmt.Fprintf(os.Stderr, "  resuming: %s (from %s, %s remaining)\n",
		remotePath, HumanSize(remoteSize), HumanSize(localSize-remoteSize))

	local, err := os.Open(localPath)
	if err != nil {
		return wrapError("resume", err)
	}
	defer local.Close()

	if _, err := local.Seek(remoteSize, io.SeekStart); err != nil {
		return wrapError("resume", err)
	}

	if err := c.conn.StorFrom(remotePath, local, uint64(remoteSize)); err != nil {
		// StorFrom not supported — full upload
		fmt.Fprintf(os.Stderr, "  warning: resume not supported by server, doing full upload\n")
		return c.Upload(localPath, remotePath)
	}

	fmt.Fprintf(os.Stderr, "  resumed: %s (+%s)\n", remotePath, HumanSize(localSize-remoteSize))
	return nil
}

// Download retrieves a remote file to a local path
func (c *ftpClient) Download(remotePath, localPath string) error {
	// Ensure local directory exists
	localDir := filepath.Dir(localPath)
	if err := os.MkdirAll(localDir, 0755); err != nil {
		return wrapError("download", err)
	}

	// Stat remote for mtime
	entry, statErr := c.conn.GetEntry(remotePath)

	// FTP Retr
	resp, err := c.conn.Retr(remotePath)
	if err != nil {
		return wrapError("download", err)
	}
	defer resp.Close()

	// Create local file
	local, err := os.Create(localPath)
	if err != nil {
		return wrapError("download", err)
	}

	n, err := io.Copy(local, resp)
	local.Close()
	if err != nil {
		// Clean up partial file
		os.Remove(localPath)
		return wrapError("download", err)
	}

	// Preserve remote mtime so next diff sees files as in sync
	if statErr == nil {
		if err := os.Chtimes(localPath, entry.Time, entry.Time); err != nil {
			fmt.Fprintf(os.Stderr, "  warning: chtimes failed for %s: %v\n", localPath, err)
		}
	}

	fmt.Fprintf(os.Stderr, "  downloaded: %s (%s)\n", localPath, HumanSize(n))
	return nil
}

// MkdirAll creates a remote directory and all parents
func (c *ftpClient) MkdirAll(remotePath string) error {
	components := strings.Split(remotePath, "/")
	current := ""

	for _, component := range components {
		if component == "" {
			current = "/"
			continue
		}
		if current == "/" {
			current = "/" + component
		} else {
			current = current + "/" + component
		}

		// Try to create — if it already exists, the error is expected
		err := c.conn.MakeDir(current)
		if err != nil {
			// FTP 550 can mean "already exists" OR "permission denied".
			// Use ChangeDir to check existence — O(1) vs a full List call.
			cwd, cwdErr := c.conn.CurrentDir()
			if cwdErr == nil {
				cdErr := c.conn.ChangeDir(current)
				// Always restore cwd; a failed restore corrupts all subsequent paths.
				if restoreErr := c.conn.ChangeDir(cwd); restoreErr != nil {
					return wrapError("mkdir", fmt.Errorf("failed to restore working directory after checking %s: %w", current, restoreErr))
				}
				if cdErr == nil {
					// Directory exists — continue to next component.
					continue
				}
			}
			return wrapError("mkdir", err)
		}
	}
	return nil
}

// Remove deletes a remote file
func (c *ftpClient) Remove(remotePath string) error {
	if err := c.conn.Delete(remotePath); err != nil {
		return wrapError("remove", err)
	}
	return nil
}

// RemoveDir deletes an empty remote directory
func (c *ftpClient) RemoveDir(remotePath string) error {
	if err := c.conn.RemoveDir(remotePath); err != nil {
		return wrapError("rmdir", err)
	}
	return nil
}

// Chmod is a no-op for FTP (no standard permission model)
func (c *ftpClient) Chmod(_ string, _ os.FileMode) error {
	return nil
}

// ftpEntryToRemote converts an ftp.Entry to RemoteFileInfo
func ftpEntryToRemote(entry *ftp.Entry, relPath string) *RemoteFileInfo {
	return &RemoteFileInfo{
		Name:    entry.Name,
		Path:    relPath,
		Size:    int64(entry.Size),
		ModTime: entry.Time,
		IsDir:   entry.Type == ftp.EntryTypeFolder,
	}
}
