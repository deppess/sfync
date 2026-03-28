package transfer

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path"
	"strings"
	"time"

	"github.com/deppess/sfync/internal/config"
)

// RemoteFileInfo represents a file on the remote server
type RemoteFileInfo struct {
	Name    string
	Path    string // relative path from remote root
	Size    int64
	ModTime time.Time
	IsDir   bool
	Mode    os.FileMode
}

// MirrorAction represents what to do with a file during mirror
type MirrorAction int

const (
	ActionUpload MirrorAction = iota
	ActionDownload
	ActionDelete
	ActionDeleteDir
	ActionMkdir
	ActionSkip
)

// MirrorEntry represents a single action in a mirror operation
type MirrorEntry struct {
	Action     MirrorAction
	RelPath    string
	LocalInfo  os.FileInfo
	RemoteInfo *RemoteFileInfo // nil for local-only files
	IsNew      bool            // true = new file, false = changed existing file
}

// MirrorResult represents the outcome of a mirror operation
type MirrorResult struct {
	Uploaded   int
	Downloaded int
	Deleted    int
	Created    int // directories created
	Skipped    int
	Warnings   []string // non-fatal errors (delete failures, chmod, etc.)
	Errors     []string // fatal errors (transfer failures after retry)
	Success    bool
	Actions    []MirrorEntry // populated in dry-run mode only
}

// RemoteClient is the interface both SFTP and FTP clients implement
type RemoteClient interface {
	Connect() error
	Close() error
	IsAlive() bool
	Stat(path string) (*RemoteFileInfo, error)
	ReadDir(path string) ([]RemoteFileInfo, error)
	Upload(localPath, remotePath string) error
	UploadResume(localPath, remotePath string) error
	Download(remotePath, localPath string) error
	MkdirAll(remotePath string) error
	Remove(remotePath string) error
	RemoveDir(remotePath string) error
	Chmod(remotePath string, mode os.FileMode) error
}

// NewClient creates the appropriate client based on protocol
func NewClient(profile *config.Profile) RemoteClient {
	if profile.Protocol == "sftp" {
		return newSFTPClient(profile)
	}
	return newFTPClient(profile)
}

// Timeouts
const (
	DialTimeout      = 10 * time.Second
	OperationTimeout = 30 * time.Second
	ReadDirTimeout   = 60 * time.Second // generous for large remote directories
	AliveTimeout     = 5 * time.Second
	ResumeThreshold  = 10 * 1024 * 1024 // 10 MB — files above this use resume
)

// TransferTimeout calculates a timeout proportional to file size.
// Base 30 seconds + 1 second per megabyte.
func TransferTimeout(fileSize int64) time.Duration {
	base := 30 * time.Second
	perMB := time.Duration(fileSize/(1024*1024)) * time.Second
	return base + perMB
}

// RemotePath joins components for a remote server path (always forward-slash).
// Use this for all remote path construction. Never use filepath.Join for remote paths.
func RemotePath(parts ...string) string {
	return path.Join(parts...)
}

// HumanSize formats a byte count into a human-readable string
func HumanSize(bytes int64) string {
	if bytes < 1024 {
		return fmt.Sprintf("%d B", bytes)
	}
	if bytes < 1024*1024 {
		return fmt.Sprintf("%.1f KB", float64(bytes)/1024)
	}
	if bytes < 1024*1024*1024 {
		return fmt.Sprintf("%.1f MB", float64(bytes)/(1024*1024))
	}
	return fmt.Sprintf("%.1f GB", float64(bytes)/(1024*1024*1024))
}

// translateError converts library errors to clean user-facing messages
func translateError(err error) string {
	if err == nil {
		return ""
	}
	msg := err.Error()

	// Connection errors
	if strings.Contains(msg, "connection refused") {
		return "Connection refused"
	}
	if strings.Contains(msg, "no such host") ||
		strings.Contains(msg, "could not resolve") {
		return "Host not found"
	}
	if errors.Is(err, context.DeadlineExceeded) ||
		strings.Contains(msg, "i/o timeout") {
		return "Connection timed out"
	}
	if strings.Contains(msg, "connection reset") {
		return "Connection reset by server"
	}
	if strings.Contains(msg, "broken pipe") {
		return "Connection lost"
	}

	// Auth errors
	if strings.Contains(msg, "unable to authenticate") ||
		strings.Contains(msg, "530") ||
		strings.Contains(msg, "Login incorrect") {
		return "Authentication failed"
	}

	// SSH key errors
	if strings.Contains(msg, "passphrase") ||
		strings.Contains(msg, "encrypted") {
		return "SSH key is passphrase-protected — use an unencrypted key or ssh-agent"
	}

	// Host key errors
	if strings.Contains(msg, "key mismatch") ||
		strings.Contains(msg, "no authority for") {
		return "Server identity changed — check ~/.ssh/known_hosts"
	}

	// File operation errors
	if strings.Contains(msg, "SSH_FX_NO_SUCH_FILE") ||
		strings.Contains(msg, "No such file") ||
		strings.Contains(msg, "550") {
		return "File or directory not found"
	}
	if strings.Contains(msg, "SSH_FX_PERMISSION_DENIED") ||
		strings.Contains(msg, "Permission denied") ||
		strings.Contains(msg, "553") {
		return "Permission denied"
	}
	if strings.Contains(msg, "disk full") ||
		strings.Contains(msg, "552") {
		return "Remote disk full"
	}
	if strings.Contains(msg, "SSH_FX_FAILURE") {
		return "Remote operation failed"
	}

	// Fallback: return raw message
	return msg
}

// wrapError logs the raw error and returns a clean one
func wrapError(operation string, err error) error {
	if err == nil {
		return nil
	}
	// Raw error goes to stderr for debugging
	fmt.Fprintf(os.Stderr, "  debug: %s: %v\n", operation, err)
	// Clean error returned to caller
	return fmt.Errorf("%s: %s", operation, translateError(err))
}
