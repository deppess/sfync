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

	"github.com/pkg/sftp"
	"golang.org/x/crypto/ssh"

	"github.com/deppess/sfync/internal/config"
)

// sftpClient implements RemoteClient for SFTP connections
type sftpClient struct {
	profile    *config.Profile
	conn       net.Conn // underlying TCP — used for SetDeadline
	sshClient  *ssh.Client
	sftpClient *sftp.Client
}

func newSFTPClient(profile *config.Profile) *sftpClient {
	return &sftpClient{profile: profile}
}

// Connect establishes the SSH + SFTP connection
func (c *sftpClient) Connect() error {
	// Safety: close existing connection if any
	if c.sftpClient != nil || c.sshClient != nil || c.conn != nil {
		c.Close()
	}

	// 1. TCP dial
	addr := net.JoinHostPort(c.profile.Host, strconv.Itoa(c.profile.Port))
	conn, err := net.DialTimeout("tcp", addr, DialTimeout)
	if err != nil {
		return wrapError("connect", err)
	}
	c.conn = conn

	// 2. Build SSH config
	sshConfig := &ssh.ClientConfig{
		User:    c.profile.Username,
		Timeout: DialTimeout,
	}

	// Authentication method
	if c.profile.SSHKey != "" {
		authMethod, err := sshKeyAuth(c.profile.SSHKey)
		if err != nil {
			c.conn.Close()
			c.conn = nil
			return wrapError("connect", err)
		}
		sshConfig.Auth = []ssh.AuthMethod{authMethod}
	} else {
		sshConfig.Auth = []ssh.AuthMethod{ssh.Password(c.profile.Password)}
	}

	// Host key verification
	verifyHostKey := true
	if c.profile.VerifyHostKey != nil {
		verifyHostKey = *c.profile.VerifyHostKey
	}
	hostKeyCallback, err := tofuCallback(verifyHostKey)
	if err != nil {
		c.conn.Close()
		c.conn = nil
		return wrapError("connect", err)
	}
	sshConfig.HostKeyCallback = hostKeyCallback

	// 3. SSH handshake over existing TCP connection
	sshConn, chans, reqs, err := ssh.NewClientConn(conn, addr, sshConfig)
	if err != nil {
		c.conn.Close()
		c.conn = nil
		return wrapError("connect", err)
	}
	c.sshClient = ssh.NewClient(sshConn, chans, reqs)

	// 4. SFTP subsystem — tuned for throughput:
	//    MaxPacketChecked(32768): 32 KB safe default, compatible with all servers
	//    UseConcurrentReads/Writes: pipeline multiple in-flight requests
	sftpConn, err := sftp.NewClient(c.sshClient,
		sftp.MaxPacketChecked(32768),
		sftp.UseConcurrentReads(true),
		sftp.UseConcurrentWrites(true),
	)
	if err != nil {
		c.sshClient.Close()
		c.sshClient = nil
		c.conn.Close()
		c.conn = nil
		return wrapError("connect", err)
	}
	c.sftpClient = sftpConn

	return nil
}

// Close disconnects all layers. Safe to call multiple times.
func (c *sftpClient) Close() error {
	var firstErr error
	if c.sftpClient != nil {
		if err := c.sftpClient.Close(); err != nil && firstErr == nil {
			firstErr = err
		}
		c.sftpClient = nil
	}
	if c.sshClient != nil {
		if err := c.sshClient.Close(); err != nil && firstErr == nil {
			firstErr = err
		}
		c.sshClient = nil
	}
	if c.conn != nil {
		if err := c.conn.Close(); err != nil && firstErr == nil {
			firstErr = err
		}
		c.conn = nil
	}
	return firstErr
}

// IsAlive checks if the connection is still usable
func (c *sftpClient) IsAlive() bool {
	if c.sftpClient == nil {
		return false
	}
	c.conn.SetDeadline(time.Now().Add(AliveTimeout))
	_, err := c.sftpClient.Stat(".")
	c.conn.SetDeadline(time.Time{})
	return err == nil
}

func (c *sftpClient) setDeadline(d time.Duration) {
	c.conn.SetDeadline(time.Now().Add(d))
}

func (c *sftpClient) clearDeadline() {
	c.conn.SetDeadline(time.Time{})
}

// Stat returns info about a remote path
func (c *sftpClient) Stat(remotePath string) (*RemoteFileInfo, error) {
	c.setDeadline(OperationTimeout)
	defer c.clearDeadline()

	info, err := c.sftpClient.Stat(remotePath)
	if err != nil {
		return nil, wrapError("stat", err)
	}
	return fileInfoToRemote(info, ""), nil
}

// ReadDir lists entries in a remote directory, skipping symlinks
func (c *sftpClient) ReadDir(remotePath string) ([]RemoteFileInfo, error) {
	c.setDeadline(ReadDirTimeout)
	defer c.clearDeadline()

	entries, err := c.sftpClient.ReadDir(remotePath)
	if err != nil {
		return nil, wrapError("readdir", err)
	}

	var result []RemoteFileInfo
	for _, entry := range entries {
		// Skip symlinks
		if entry.Mode()&os.ModeSymlink != 0 {
			continue
		}
		result = append(result, *fileInfoToRemote(entry, ""))
	}
	return result, nil
}

// Upload streams a local file to the remote server
func (c *sftpClient) Upload(localPath, remotePath string) error {
	// Open and stat local file
	local, err := os.Open(localPath)
	if err != nil {
		return wrapError("upload", err)
	}
	defer local.Close()

	localInfo, err := local.Stat()
	if err != nil {
		return wrapError("upload", err)
	}

	// Set deadline proportional to file size
	c.setDeadline(TransferTimeout(localInfo.Size()))
	defer c.clearDeadline()

	// Ensure remote directory exists
	remoteDir := path.Dir(remotePath)
	if err := c.MkdirAll(remoteDir); err != nil {
		return wrapError("upload", fmt.Errorf("cannot create directory %s: %w", remoteDir, err))
	}

	// Create remote file
	remote, err := c.sftpClient.Create(remotePath)
	if err != nil {
		return wrapError("upload", err)
	}
	defer remote.Close()

	// Stream copy
	n, err := io.Copy(remote, local)
	if err != nil {
		return wrapError("upload", err)
	}

	// Preserve permissions (non-fatal)
	if chmodErr := c.sftpClient.Chmod(remotePath, localInfo.Mode()); chmodErr != nil {
		fmt.Fprintf(os.Stderr, "  warning: chmod failed for %s: %v\n", remotePath, chmodErr)
	}

	// Preserve mtime so next diff sees files as in sync
	if chtErr := c.sftpClient.Chtimes(remotePath, localInfo.ModTime(), localInfo.ModTime()); chtErr != nil {
		fmt.Fprintf(os.Stderr, "  warning: chtimes failed for %s: %v\n", remotePath, chtErr)
	}

	fmt.Fprintf(os.Stderr, "  uploaded: %s (%s)\n", remotePath, HumanSize(n))
	return nil
}

// UploadResume uploads a file with resume support for large files
func (c *sftpClient) UploadResume(localPath, remotePath string) error {
	// Stat local file
	localInfo, err := os.Stat(localPath)
	if err != nil {
		return wrapError("resume", err)
	}
	localSize := localInfo.Size()

	// Stat remote file
	c.setDeadline(OperationTimeout)
	remoteInfo, statErr := c.sftpClient.Stat(remotePath)
	c.clearDeadline()

	if statErr != nil {
		// Remote doesn't exist — full upload
		return c.Upload(localPath, remotePath)
	}

	remoteSize := remoteInfo.Size()

	// Decide action
	if remoteSize == localSize {
		fmt.Fprintf(os.Stderr, "  skipped (already complete): %s\n", remotePath)
		return nil
	}
	if remoteSize > localSize {
		// Remote is larger — corrupt or different file, full re-upload
		fmt.Fprintf(os.Stderr, "  re-uploading (remote larger than local): %s\n", remotePath)
		return c.Upload(localPath, remotePath)
	}

	// Resume: remote is smaller than local
	fmt.Fprintf(os.Stderr, "  resuming: %s (from %s, %s remaining)\n",
		remotePath, HumanSize(remoteSize), HumanSize(localSize-remoteSize))

	c.setDeadline(TransferTimeout(localSize - remoteSize))
	defer c.clearDeadline()

	// Open local, seek past already-uploaded portion
	local, err := os.Open(localPath)
	if err != nil {
		return wrapError("resume", err)
	}
	defer local.Close()

	if _, err := local.Seek(remoteSize, io.SeekStart); err != nil {
		return wrapError("resume", err)
	}

	// Open remote for writing and seek to end
	remote, err := c.sftpClient.OpenFile(remotePath, os.O_WRONLY)
	if err != nil {
		// Can't open for append — fall back to full upload
		fmt.Fprintf(os.Stderr, "  warning: open for append failed, doing full upload\n")
		return c.Upload(localPath, remotePath)
	}
	defer remote.Close()

	if _, err := remote.Seek(0, io.SeekEnd); err != nil {
		fmt.Fprintf(os.Stderr, "  warning: seek failed, doing full upload\n")
		return c.Upload(localPath, remotePath)
	}

	// Stream remaining bytes
	n, err := io.Copy(remote, local)
	if err != nil {
		return wrapError("resume", err)
	}

	// Preserve permissions and mtime (same as Upload, to keep diff clean)
	if chmodErr := c.sftpClient.Chmod(remotePath, localInfo.Mode()); chmodErr != nil {
		fmt.Fprintf(os.Stderr, "  warning: chmod failed for %s: %v\n", remotePath, chmodErr)
	}
	if chtErr := c.sftpClient.Chtimes(remotePath, localInfo.ModTime(), localInfo.ModTime()); chtErr != nil {
		fmt.Fprintf(os.Stderr, "  warning: chtimes failed for %s: %v\n", remotePath, chtErr)
	}

	fmt.Fprintf(os.Stderr, "  resumed: %s (+%s)\n", remotePath, HumanSize(n))
	return nil
}

// Download retrieves a remote file to a local path
func (c *sftpClient) Download(remotePath, localPath string) error {
	// Ensure local directory exists
	localDir := filepath.Dir(localPath)
	if err := os.MkdirAll(localDir, 0755); err != nil {
		return wrapError("download", err)
	}

	// Stat remote for size and deadline
	c.setDeadline(OperationTimeout)
	remoteInfo, err := c.sftpClient.Stat(remotePath)
	c.clearDeadline()
	if err != nil {
		return wrapError("download", err)
	}

	c.setDeadline(TransferTimeout(remoteInfo.Size()))
	defer c.clearDeadline()

	// Open remote
	remote, err := c.sftpClient.Open(remotePath)
	if err != nil {
		return wrapError("download", err)
	}
	defer remote.Close()

	// Create local (truncate if exists)
	local, err := os.Create(localPath)
	if err != nil {
		return wrapError("download", err)
	}

	// Stream
	n, err := io.Copy(local, remote)
	local.Close()
	if err != nil {
		// Clean up partial local file on failure
		os.Remove(localPath)
		return wrapError("download", err)
	}

	// Preserve remote mtime on local file
	if err := os.Chtimes(localPath, remoteInfo.ModTime(), remoteInfo.ModTime()); err != nil {
		fmt.Fprintf(os.Stderr, "  warning: chtimes failed for %s: %v\n", localPath, err)
	}

	fmt.Fprintf(os.Stderr, "  downloaded: %s (%s)\n", localPath, HumanSize(n))
	return nil
}

// MkdirAll creates a remote directory and all parents
func (c *sftpClient) MkdirAll(remotePath string) error {
	// SFTP has no native MkdirAll — walk components.
	// Each Stat+Mkdir pair gets its own deadline so a deep directory tree
	// cannot exhaust a single shared budget.
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

		// Check if exists
		c.setDeadline(OperationTimeout)
		_, statErr := c.sftpClient.Stat(current)
		c.clearDeadline()
		if statErr == nil {
			continue
		}

		// Try to create
		c.setDeadline(OperationTimeout)
		mkErr := c.sftpClient.Mkdir(current)
		c.clearDeadline()
		if mkErr != nil {
			// Race: check if created between Stat and Mkdir
			c.setDeadline(OperationTimeout)
			_, raceStatErr := c.sftpClient.Stat(current)
			c.clearDeadline()
			if raceStatErr == nil {
				continue
			}
			return wrapError("mkdir", mkErr)
		}
	}
	return nil
}

// Remove deletes a remote file
func (c *sftpClient) Remove(remotePath string) error {
	c.setDeadline(OperationTimeout)
	defer c.clearDeadline()

	if err := c.sftpClient.Remove(remotePath); err != nil {
		return wrapError("remove", err)
	}
	return nil
}

// RemoveDir deletes an empty remote directory
func (c *sftpClient) RemoveDir(remotePath string) error {
	c.setDeadline(OperationTimeout)
	defer c.clearDeadline()

	if err := c.sftpClient.RemoveDirectory(remotePath); err != nil {
		return wrapError("rmdir", err)
	}
	return nil
}

// Chmod sets permissions on a remote file
func (c *sftpClient) Chmod(remotePath string, mode os.FileMode) error {
	c.setDeadline(OperationTimeout)
	defer c.clearDeadline()

	if err := c.sftpClient.Chmod(remotePath, mode); err != nil {
		return wrapError("chmod", err)
	}
	return nil
}

// sshKeyAuth reads an SSH private key and returns an auth method
func sshKeyAuth(keyPath string) (ssh.AuthMethod, error) {
	keyBytes, err := os.ReadFile(keyPath)
	if err != nil {
		return nil, fmt.Errorf("SSH key not found: %s", keyPath)
	}

	key, err := ssh.ParsePrivateKey(keyBytes)
	if err != nil {
		if _, ok := err.(*ssh.PassphraseMissingError); ok {
			return nil, fmt.Errorf("SSH key is passphrase-protected: %s", keyPath)
		}
		return nil, fmt.Errorf("cannot parse SSH key %s: %w", keyPath, err)
	}

	return ssh.PublicKeys(key), nil
}

// fileInfoToRemote converts an os.FileInfo to a RemoteFileInfo
func fileInfoToRemote(info os.FileInfo, relPath string) *RemoteFileInfo {
	return &RemoteFileInfo{
		Name:    info.Name(),
		Path:    relPath,
		Size:    info.Size(),
		ModTime: info.ModTime(),
		IsDir:   info.IsDir(),
		Mode:    info.Mode(),
	}
}
