package transfer

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"

	"github.com/pkg/sftp"
	"golang.org/x/crypto/ssh"

	"github.com/deppess/sfync/internal/config"
)

// sftpClient implements RemoteClient for SFTP connections
type sftpClient struct {
	profile    *config.Profile
	conn       net.Conn         // underlying TCP — its deadline is managed by deadlines
	deadlines  *deadlineTracker // per-operation timeouts on the shared conn
	sshClient  *ssh.Client
	sftpClient *sftp.Client // primary session: metadata, scans, fallback for transfers

	// xfer holds extra SFTP sessions (channels on the same SSH connection)
	// that file transfers check out one each — see transferSession.
	xfer         *connPool[*sftp.Client]
	xferDisabled atomic.Bool // server allows no extra session; use primary
	packetSize   int
}

func newSFTPClient(profile *config.Profile) *sftpClient {
	return &sftpClient{profile: profile}
}

// SupportsConcurrentRequests implements concurrentClient (see client.go) —
// pkg/sftp's Client is documented safe to call from multiple goroutines.
func (c *sftpClient) SupportsConcurrentRequests() bool { return true }

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
	c.deadlines = newDeadlineTracker(conn)

	// ssh.ClientConfig.Timeout only bounds ssh.Dial's own TCP dial — it does
	// not apply to a handshake over a conn we dialed ourselves — so bound the
	// SSH handshake and SFTP subsystem negotiation explicitly. Without this a
	// server that accepts TCP but never speaks would hang Connect forever.
	endHandshake := c.deadlines.begin(DialTimeout)
	defer endHandshake()

	// 2. Build SSH config
	sshConfig := &ssh.ClientConfig{
		User:    c.profile.Username,
		Timeout: DialTimeout,
	}

	// Authentication method
	if c.profile.SSHKey != "" {
		// The agent connection is only used for signing during the
		// handshake below, so it can be closed when Connect returns.
		ag, closeAgent := dialAgent()
		defer closeAgent()
		signers, err := sshKeySigners(c.profile.SSHKey, ag)
		if err != nil {
			c.conn.Close()
			c.conn = nil
			return wrapError("connect", err)
		}
		sshConfig.Auth = []ssh.AuthMethod{ssh.PublicKeys(signers...)}
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

	// 4. SFTP subsystem — tuned for throughput (see sftpOptions)
	c.packetSize = sftpPacketSize(c.sshClient.ServerVersion())
	sftpConn, err := sftp.NewClient(c.sshClient, c.sftpOptions()...)
	if err != nil {
		c.sshClient.Close()
		c.sshClient = nil
		c.conn.Close()
		c.conn = nil
		return wrapError("connect", err)
	}
	c.sftpClient = sftpConn

	c.xferDisabled.Store(false)
	c.xfer = newConnPool(MaxConcurrentTransfers,
		func() (*sftp.Client, error) { return sftp.NewClient(c.sshClient, c.sftpOptions()...) },
		func(s *sftp.Client) { s.Close() },
		func(limit int) {
			fmt.Fprintf(os.Stderr, "  note: %s allowed only %d extra SFTP session(s); continuing with %d\n",
				c.profile.Host, limit, limit)
		})

	return nil
}

// Packet sizes for SFTP reads and writes. 32 KB is the size every server
// must accept. OpenSSH's sftp-server accepts messages up to 256 KB and has
// for well over a decade, and larger packets mean fewer requests per byte —
// measurably less CPU on fast links (testenv, local OpenSSH, 113 MB:
// downloads 694 → 1021 MiB/s, uploads 579 → 757 MiB/s at 128 KB) — so
// servers identifying as OpenSSH get openSSHPacketSize, half the 256 KB
// limit for headroom. Anything else stays at the universal size, since some
// SFTP servers reject larger requests and pkg/sftp can't query a server's
// limits.
const (
	defaultSFTPPacketSize = 32 * 1024
	openSSHPacketSize     = 128 * 1024
)

// sftpPacketSize picks the packet size for an SSH server version banner
// (e.g. "SSH-2.0-OpenSSH_9.6").
func sftpPacketSize(serverVersion []byte) int {
	if strings.HasPrefix(string(serverVersion), "SSH-2.0-OpenSSH_") {
		return openSSHPacketSize
	}
	return defaultSFTPPacketSize
}

// sftpOptions configures every SFTP session: the packet size, and
// pipelining of many in-flight requests per file (UseConcurrentReads/
// Writes).
func (c *sftpClient) sftpOptions() []sftp.ClientOption {
	packet := sftp.MaxPacketChecked(defaultSFTPPacketSize)
	if c.packetSize > defaultSFTPPacketSize {
		packet = sftp.MaxPacketUnchecked(c.packetSize)
	}
	return []sftp.ClientOption{packet, sftp.UseConcurrentReads(true), sftp.UseConcurrentWrites(true)}
}

// transferSession returns an SFTP session for one file transfer and a
// release function to call with the transfer's result.
//
// Each concurrent transfer gets its own session — its own SSH channel on
// the same connection. An SSH channel carries at most its flow-control
// window (~2 MB in x/crypto/ssh) in flight, so on a fast, high-latency link
// a single shared session caps all parallel transfers together at roughly
// window/RTT (≈50 MB/s at 40ms); separate channels each get their own
// window. Extra sessions open lazily and are reused. Servers limit channels
// per connection (OpenSSH MaxSessions, default 10): if one can't be opened
// the pool stops growing, and if none can, transfers share the primary
// session, as before.
func (c *sftpClient) transferSession() (*sftp.Client, func(error)) {
	if c.xfer != nil && !c.xferDisabled.Load() {
		s, err := c.xfer.acquire()
		if err == nil {
			return s, func(err error) { c.xfer.release(s, sftpSessionBroken(err)) }
		}
		if !errors.Is(err, errPoolClosed) && c.xferDisabled.CompareAndSwap(false, true) {
			fmt.Fprintf(os.Stderr, "  note: %s allows no extra SFTP sessions; transfers share one\n", c.profile.Host)
		}
	}
	return c.sftpClient, func(error) {}
}

// sftpSessionBroken reports whether an SFTP session must be discarded after
// an operation returned err. A status reply from the server (no such file,
// permission denied) or a local file error leaves the session usable;
// anything else (the channel or connection failing) does not.
func sftpSessionBroken(err error) bool {
	if err == nil || errors.Is(err, fs.ErrNotExist) || errors.Is(err, fs.ErrPermission) {
		return false
	}
	if _, ok := errors.AsType[*sftp.StatusError](err); ok {
		return false
	}
	_, isPathErr := errors.AsType[*fs.PathError](err)
	return !isPathErr
}

// Close disconnects all layers. Safe to call multiple times.
func (c *sftpClient) Close() error {
	var firstErr error
	if c.xfer != nil {
		c.xfer.closeAll()
		c.xfer = nil
	}
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
	defer c.deadlines.begin(AliveTimeout)()
	_, err := c.sftpClient.Stat(".")
	return err == nil
}

// Stat returns info about a remote path
func (c *sftpClient) Stat(remotePath string) (*RemoteFileInfo, error) {
	defer c.deadlines.begin(OperationTimeout)()

	info, err := c.sftpClient.Stat(remotePath)
	if err != nil {
		return nil, wrapError("stat", err)
	}
	return fileInfoToRemote(info, ""), nil
}

// ReadDir lists entries in a remote directory, skipping symlinks
func (c *sftpClient) ReadDir(remotePath string) ([]RemoteFileInfo, error) {
	defer c.deadlines.begin(ReadDirTimeout)()

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
		// Reject entry names that aren't a plain path component — see
		// isSafeEntryName for why a server can't be trusted here.
		if !isSafeEntryName(entry.Name()) {
			fmt.Fprintf(os.Stderr, "  warning: skipping unsafe remote entry name %q in %s\n", entry.Name(), remotePath)
			continue
		}
		result = append(result, *fileInfoToRemote(entry, ""))
	}
	return result, nil
}

// Upload streams a local file to the remote server. The remote directory
// must already exist — callers are responsible for calling MkdirAll first
// (mirror.go pre-creates directories in a dedicated phase before any
// transfers run; single-file callers like Push and the auto-sync queue call
// MkdirAll explicitly before Upload/UploadResume).
//
// The upload is atomic: data goes to a hidden partial file beside
// remotePath, which is renamed over remotePath only once complete — so the
// live file is never left truncated by a failed transfer.
func (c *sftpClient) Upload(localPath, remotePath string) error {
	return c.upload(localPath, remotePath, false)
}

// UploadResume is Upload for large files: a partial left behind by an
// earlier failed attempt is continued instead of restarted (when it is still
// valid — see canResumePartial), and a failed attempt keeps its partial for
// the next try.
func (c *sftpClient) UploadResume(localPath, remotePath string) error {
	return c.upload(localPath, remotePath, true)
}

func (c *sftpClient) upload(localPath, remotePath string, resume bool) error {
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

	sess, release := c.transferSession()
	err = c.uploadOn(sess, op, local, localInfo, remotePath, resume)
	release(err)
	return err
}

// uploadOn performs upload's transfer on one SFTP session.
func (c *sftpClient) uploadOn(sess *sftp.Client, op string, local *os.File, localInfo os.FileInfo, remotePath string, resume bool) error {
	partial := remotePartialPath(remotePath)

	var offset int64
	if resume {
		endOp := c.deadlines.begin(OperationTimeout)
		partialInfo, statErr := sess.Stat(partial)
		endOp()
		if statErr == nil && canResumePartial(partialInfo.Size(), partialInfo.ModTime(), localInfo) {
			offset = partialInfo.Size()
		}
	}

	// Set deadline proportional to the bytes left to send
	defer c.deadlines.begin(TransferTimeout(localInfo.Size() - offset))()

	flags := os.O_WRONLY | os.O_CREATE
	if offset == 0 {
		flags |= os.O_TRUNC
	}
	remote, err := sess.OpenFile(partial, flags)
	if err != nil {
		return wrapError(op, err)
	}

	if offset > 0 {
		fmt.Fprintf(os.Stderr, "  resuming: %s (from %s, %s remaining)\n",
			remotePath, HumanSize(offset), HumanSize(localInfo.Size()-offset))
		if _, err = local.Seek(offset, io.SeekStart); err == nil {
			_, err = remote.Seek(offset, io.SeekStart)
		}
	}

	// Stream copy. Close errors matter here: with concurrent writes enabled,
	// a write failure can surface only when the handle is closed.
	var n int64
	if err == nil {
		n, err = io.Copy(remote, local)
	}
	if cerr := remote.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		if !resume {
			c.removeQuiet(sess, partial)
		}
		return wrapError(op, err)
	}

	// Preserve permissions (non-fatal)
	if chmodErr := sess.Chmod(partial, localInfo.Mode()); chmodErr != nil {
		fmt.Fprintf(os.Stderr, "  warning: chmod failed for %s: %v\n", remotePath, chmodErr)
	}

	// Preserve mtime so next diff sees files as in sync
	if chtErr := sess.Chtimes(partial, localInfo.ModTime(), localInfo.ModTime()); chtErr != nil {
		fmt.Fprintf(os.Stderr, "  warning: chtimes failed for %s: %v\n", remotePath, chtErr)
	}

	if err := sftpReplace(sess, partial, remotePath); err != nil {
		c.removeQuiet(sess, partial)
		return wrapError(op, err)
	}

	if offset > 0 {
		fmt.Fprintf(os.Stderr, "  resumed: %s (+%s)\n", remotePath, HumanSize(n))
	} else {
		fmt.Fprintf(os.Stderr, "  uploaded: %s (%s)\n", remotePath, HumanSize(n))
	}
	return nil
}

// replace renames src over dst. OpenSSH's posix-rename extension does this
// atomically. Plain SFTP v3 rename refuses to overwrite an existing target,
// so without the extension dst is removed first — leaving only a brief
// window where dst is missing, never one where it is truncated.
func sftpReplace(sess *sftp.Client, src, dst string) error {
	if _, ok := sess.HasExtension("posix-rename@openssh.com"); ok {
		return sess.PosixRename(src, dst)
	}
	if err := sess.Rename(src, dst); err == nil {
		return nil
	}
	if err := sess.Remove(dst); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return sess.Rename(src, dst)
}

// removeQuiet best-effort deletes a remote file (used to clean up partials).
func (c *sftpClient) removeQuiet(sess *sftp.Client, remotePath string) {
	defer c.deadlines.begin(OperationTimeout)()
	sess.Remove(remotePath)
}

// Download retrieves a remote file to a local path. The local file is only
// replaced once the download has fully succeeded (see writeLocalAtomic).
func (c *sftpClient) Download(remotePath, localPath string) error {
	// Ensure local directory exists
	localDir := filepath.Dir(localPath)
	if err := os.MkdirAll(localDir, 0755); err != nil {
		return wrapError("download", err)
	}

	sess, release := c.transferSession()
	err := c.downloadOn(sess, remotePath, localPath)
	release(err)
	return err
}

// downloadOn performs Download's transfer on one SFTP session.
func (c *sftpClient) downloadOn(sess *sftp.Client, remotePath, localPath string) error {
	// Stat remote for size and deadline
	endOp := c.deadlines.begin(OperationTimeout)
	remoteInfo, err := sess.Stat(remotePath)
	endOp()
	if err != nil {
		return wrapError("download", err)
	}

	defer c.deadlines.begin(TransferTimeout(remoteInfo.Size()))()

	// Open remote
	remote, err := sess.Open(remotePath)
	if err != nil {
		return wrapError("download", err)
	}
	defer remote.Close()

	// Stream to a temp file and rename into place, preserving remote mtime
	n, err := writeLocalAtomic(localPath, remote, nil, remoteInfo.ModTime())
	if err != nil {
		return wrapError("download", err)
	}

	fmt.Fprintf(os.Stderr, "  downloaded: %s (%s)\n", localPath, HumanSize(n))
	return nil
}

// Mkdir creates a single remote directory whose parent already exists.
func (c *sftpClient) Mkdir(remotePath string) error {
	defer c.deadlines.begin(OperationTimeout)()

	if err := c.sftpClient.Mkdir(remotePath); err != nil {
		return wrapError("mkdir", err)
	}
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
		endOp := c.deadlines.begin(OperationTimeout)
		_, statErr := c.sftpClient.Stat(current)
		endOp()
		if statErr == nil {
			continue
		}

		// Try to create
		endOp = c.deadlines.begin(OperationTimeout)
		mkErr := c.sftpClient.Mkdir(current)
		endOp()
		if mkErr != nil {
			// Race: check if created between Stat and Mkdir
			endOp := c.deadlines.begin(OperationTimeout)
			_, raceStatErr := c.sftpClient.Stat(current)
			endOp()
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
	defer c.deadlines.begin(OperationTimeout)()

	if err := c.sftpClient.Remove(remotePath); err != nil {
		return wrapError("remove", err)
	}
	return nil
}

// RemoveDir deletes an empty remote directory
func (c *sftpClient) RemoveDir(remotePath string) error {
	defer c.deadlines.begin(OperationTimeout)()

	if err := c.sftpClient.RemoveDirectory(remotePath); err != nil {
		return wrapError("rmdir", err)
	}
	return nil
}

// Chmod sets permissions on a remote file
func (c *sftpClient) Chmod(remotePath string, mode os.FileMode) error {
	defer c.deadlines.begin(OperationTimeout)()

	if err := c.sftpClient.Chmod(remotePath, mode); err != nil {
		return wrapError("chmod", err)
	}
	return nil
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
