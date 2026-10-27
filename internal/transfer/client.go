package transfer

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"net/textproto"
	"os"
	"path"
	"strings"
	"time"

	"github.com/jlaffaye/ftp"

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
	ActionConflict // same path is a file on one side and a directory on the other
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
	Mkdir(remotePath string) error // single directory; parent must exist
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

	// MaxConcurrentReadDir bounds how many ReadDir calls walkRemote issues in
	// parallel when the underlying client supports concurrent requests (SFTP
	// only — see walkRemote in mirror.go).
	MaxConcurrentReadDir = 8

	// MaxConcurrentTransfers bounds how many file uploads/downloads run in
	// parallel over one SFTP connection (see runTransfers in mirror.go).
	// Kept smaller than MaxConcurrentReadDir since transfers are bandwidth-
	// bound, not just latency-bound — over-parallelizing large files buys
	// nothing and just contends for the same link.
	MaxConcurrentTransfers = 4
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

// concurrentClient is implemented by RemoteClient implementations whose
// requests are safe to issue from multiple goroutines simultaneously.
// pkg/sftp's Client documents this guarantee, so sftpClient implements it;
// FTP's single control connection allows only one command in flight at a
// time, so ftpClient does not. walkRemote and runTransfers (mirror.go) use
// this to decide whether to parallelize directory listing / transfers.
type concurrentClient interface {
	SupportsConcurrentRequests() bool
}

// supportsConcurrentRequests reports whether client is safe to call from
// multiple goroutines at once, defaulting to false for any client that
// doesn't explicitly claim otherwise via concurrentClient.
func supportsConcurrentRequests(client RemoteClient) bool {
	cc, ok := client.(concurrentClient)
	return ok && cc.SupportsConcurrentRequests()
}

// isSafeEntryName reports whether a directory-entry name returned by the
// remote server is safe to treat as a single path component. Protocol-level
// filenames are attacker-controlled input from the server's point of view —
// a malicious or compromised SFTP/FTP server can return an entry named
// e.g. "../../../home/user/.ssh/authorized_keys" in a directory listing.
// Every consumer of ReadDir joins entry names into local (mirror down) and
// remote (mirror up) paths, so entries that aren't a plain single component
// are rejected here, at the source, rather than trusted downstream — the
// same class of bug as "zip slip" in archive extractors.
func isSafeEntryName(name string) bool {
	if name == "" || name == "." || name == ".." {
		return false
	}
	return !strings.ContainsAny(name, "/\\")
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

	// TLS errors (ftps)
	if strings.Contains(msg, "x509:") || strings.Contains(msg, "tls:") {
		return "TLS error: " + msg
	}

	// Auth errors
	if strings.Contains(msg, "unable to authenticate") ||
		strings.Contains(msg, "530") ||
		strings.Contains(msg, "Login incorrect") {
		return "Authentication failed"
	}

	// SSH key errors — sshKeySigners already produces actionable messages
	// (including the exact ssh-add command), so pass them through as-is.
	if strings.Contains(msg, "passphrase-protected") {
		return msg
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
	return &cleanError{msg: fmt.Sprintf("%s: %s", operation, translateError(err)), err: err}
}

// cleanError carries the translated, user-facing message while keeping the
// raw library error reachable via errors.Is/As — e.g. so callers can test
// errors.Is(err, fs.ErrNotExist) instead of matching message text.
type cleanError struct {
	msg string
	err error
}

func (e *cleanError) Error() string { return e.msg }
func (e *cleanError) Unwrap() error { return e.err }

// IsNotExist reports whether err from a RemoteClient means the remote path
// doesn't exist. SFTP errors carry fs.ErrNotExist (pkg/sftp normalizes
// SSH_FX_NO_SUCH_FILE). FTP only has reply code 550, which some servers
// also use for permission errors — callers use this to decide *not* to act,
// so erring toward "doesn't exist" is the safe direction.
func IsNotExist(err error) bool {
	if errors.Is(err, fs.ErrNotExist) {
		return true
	}
	var tpErr *textproto.Error
	return errors.As(err, &tpErr) && tpErr.Code == ftp.StatusFileUnavailable
}
