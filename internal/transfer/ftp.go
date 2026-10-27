package transfer

import (
	"crypto/tls"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
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

// Connect establishes the FTP connection and logs in. protocol "ftps" adds
// explicit AUTH TLS (RFC 4217) — the control connection starts in
// plaintext, then upgrades to TLS before the login exchange, so credentials
// and file contents never go over the wire unencrypted.
func (c *ftpClient) Connect() error {
	// Safety: close existing connection if any
	if c.conn != nil {
		c.Close()
	}

	addr := net.JoinHostPort(c.profile.Host, strconv.Itoa(c.profile.Port))

	dialOpts := []ftp.DialOption{ftp.DialWithTimeout(DialTimeout)}
	if c.profile.Protocol == "ftps" {
		dialOpts = append(dialOpts, ftp.DialWithExplicitTLS(&tls.Config{ServerName: c.profile.Host}))
	}

	conn, err := ftp.Dial(addr, dialOpts...)
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
		// Skip symlinks
		if entry.Type == ftp.EntryTypeLink {
			continue
		}
		// Reject entry names that aren't a plain path component (also
		// covers "." and ".."). See isSafeEntryName for why a server
		// can't be trusted here.
		if !isSafeEntryName(entry.Name) {
			fmt.Fprintf(os.Stderr, "  warning: skipping unsafe remote entry name %q in %s\n", entry.Name, remotePath)
			continue
		}
		result = append(result, *ftpEntryToRemote(entry, ""))
	}
	return result, nil
}

// Upload streams a local file to the remote server. The remote directory
// must already exist — callers are responsible for calling MkdirAll first
// (mirror.go pre-creates directories in a dedicated phase before any
// transfers run; single-file callers like Push and the auto-sync queue call
// MkdirAll explicitly before Upload/UploadResume).
//
// Like sftpClient.Upload, the upload goes to a hidden partial file that is
// renamed over remotePath only once complete.
func (c *ftpClient) Upload(localPath, remotePath string) error {
	return c.upload(localPath, remotePath, false)
}

// UploadResume is Upload for large files: a still-valid partial from an
// earlier failed attempt is continued (REST + STOR) instead of restarted,
// and a failed attempt keeps its partial for the next try.
func (c *ftpClient) UploadResume(localPath, remotePath string) error {
	return c.upload(localPath, remotePath, true)
}

func (c *ftpClient) upload(localPath, remotePath string, resume bool) error {
	op := "upload"
	if resume {
		op = "resume"
	}

	local, err := os.Open(localPath)
	if err != nil {
		return wrapError(op, err)
	}
	defer local.Close()

	localInfo, err := local.Stat()
	if err != nil {
		return wrapError(op, err)
	}

	partial := remotePartialPath(remotePath)

	var offset int64
	if resume {
		size, sizeErr := c.conn.FileSize(partial)
		if sizeErr == nil {
			// No MDTM support means we can't tell whether the partial is
			// from the current version of the file — restart to be safe.
			if mtime, timeErr := c.conn.GetTime(partial); timeErr == nil &&
				canResumePartial(size, mtime, localInfo) {
				offset = size
			}
		}
	}

	if offset > 0 {
		fmt.Fprintf(os.Stderr, "  resuming: %s (from %s, %s remaining)\n",
			remotePath, HumanSize(offset), HumanSize(localInfo.Size()-offset))
		if _, err = local.Seek(offset, io.SeekStart); err == nil {
			err = c.conn.StorFrom(partial, local, uint64(offset))
		}
		if err != nil {
			// REST unsupported or the partial is unusable — restart from zero
			fmt.Fprintf(os.Stderr, "  warning: resume failed (%s), doing full upload\n", translateError(err))
			offset = 0
		}
	}
	if offset == 0 {
		if _, err = local.Seek(0, io.SeekStart); err == nil {
			err = c.conn.Stor(partial, local)
		}
	}
	if err != nil {
		if !resume {
			c.conn.Delete(partial)
		}
		return wrapError(op, err)
	}

	// Preserve mtime (MFMT, or vsftpd's writable MDTM) so the next diff sees
	// the file as in sync. Without it the remote mtime is the upload time and
	// every subsequent `sfync up` would re-upload every file. Rename keeps
	// the mtime, so set it on the partial before swapping it in.
	if c.conn.IsSetTimeSupported() {
		if err := c.conn.SetTime(partial, localInfo.ModTime()); err != nil {
			fmt.Fprintf(os.Stderr, "  warning: set mtime failed for %s: %v\n", remotePath, err)
		}
	} else {
		warnNoSetTime(c.profile.Host)
	}

	if err := c.replace(partial, remotePath); err != nil {
		c.conn.Delete(partial)
		return wrapError(op, err)
	}

	fmt.Fprintf(os.Stderr, "  uploaded: %s (%s)\n", remotePath, HumanSize(localInfo.Size()))
	return nil
}

// replace renames src over dst. Most Unix FTP servers overwrite on RNTO;
// servers that refuse (e.g. IIS) get dst deleted first, leaving only a brief
// window where dst is missing, never one where it is truncated.
func (c *ftpClient) replace(src, dst string) error {
	if err := c.conn.Rename(src, dst); err == nil {
		return nil
	}
	c.conn.Delete(dst) // may fail if dst doesn't exist — the retry decides
	return c.conn.Rename(src, dst)
}

// noSetTimeWarned records hosts already warned about missing MFMT support,
// so a mirror of many files warns once rather than per file.
var noSetTimeWarned sync.Map

func warnNoSetTime(host string) {
	if _, already := noSetTimeWarned.LoadOrStore(host, true); !already {
		fmt.Fprintf(os.Stderr,
			"  warning: %s does not support setting file times (MFMT) — "+
				"uploaded files will look changed to the next sync\n", host)
	}
}

// Download retrieves a remote file to a local path. The local file is only
// replaced once the download has fully succeeded (see writeLocalAtomic).
func (c *ftpClient) Download(remotePath, localPath string) error {
	// Ensure local directory exists
	localDir := filepath.Dir(localPath)
	if err := os.MkdirAll(localDir, 0755); err != nil {
		return wrapError("download", err)
	}

	// Stat remote for mtime
	var mtime time.Time
	if entry, statErr := c.conn.GetEntry(remotePath); statErr == nil {
		mtime = entry.Time
	}

	// FTP Retr
	resp, err := c.conn.Retr(remotePath)
	if err != nil {
		return wrapError("download", err)
	}
	defer resp.Close() // idempotent; writeLocalAtomic closes it first

	// resp.Close is passed as finish: it reads the server's end-of-transfer
	// status, so a transfer the server aborted is never committed as complete.
	n, err := writeLocalAtomic(localPath, resp, resp.Close, mtime)
	if err != nil {
		return wrapError("download", err)
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
