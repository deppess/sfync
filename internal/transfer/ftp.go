package transfer

import (
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/textproto"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/jlaffaye/ftp"

	"github.com/deppess/sfync/internal/config"
)

// ftpClient implements RemoteClient for FTP connections.
//
// FTP carries one command at a time per control connection, so parallelism
// comes from a pool of connections (profile.FTPConnections; see connPool).
// Every operation checks out one connection for its whole duration — its
// session state (working directory, transfer type) is never shared between
// concurrent operations.
type ftpClient struct {
	profile   *config.Profile
	pool      *connPool[*ftpSession]
	tlsConfig *tls.Config // shared by all connections; nil for plain FTP

	// LIST timestamps carry no timezone; many servers list in their local
	// time. listOffset is how far the server's LIST times are ahead of UTC,
	// detected once per client (see detectListOffset). Shared by all pooled
	// connections — they talk to the same server.
	offsetMu        sync.Mutex
	listOffset      time.Duration
	listOffsetKnown bool
	listOffsetTries int
}

func newFTPClient(profile *config.Profile) *ftpClient {
	return &ftpClient{profile: profile}
}

// SupportsConcurrentRequests implements concurrentClient (see client.go):
// with more than one pooled connection, operations can run in parallel.
func (c *ftpClient) SupportsConcurrentRequests() bool {
	return c.profile.FTPConnections > 1
}

// Connect opens the first connection and logs in, so bad credentials or an
// unreachable server fail immediately; further connections open lazily as
// parallel work needs them. protocol "ftps" adds explicit AUTH TLS
// (RFC 4217) — each control connection starts in plaintext, then upgrades
// to TLS before the login exchange, so credentials and file contents never
// go over the wire unencrypted.
func (c *ftpClient) Connect() error {
	if c.pool != nil {
		c.Close()
	}

	c.tlsConfig = nil
	if c.profile.Protocol == "ftps" {
		cfg, err := ftpTLSConfig(c.profile)
		if err != nil {
			return wrapError("connect", err)
		}
		c.tlsConfig = cfg
	}

	conn, err := c.dial()
	if err != nil {
		return wrapError("connect", err)
	}

	c.pool = newConnPool(max(c.profile.FTPConnections, 1), c.dial,
		func(s *ftpSession) { s.close() },
		func(limit int) {
			fmt.Fprintf(os.Stderr, "  note: %s accepted only %d parallel FTP connection(s); continuing with %d\n",
				c.profile.Host, limit, limit)
		})
	c.pool.seed(conn)

	c.offsetMu.Lock()
	c.listOffset, c.listOffsetKnown, c.listOffsetTries = 0, false, 0
	c.offsetMu.Unlock()
	return nil
}

// dial opens and authenticates one control connection.
func (c *ftpClient) dial() (*ftpSession, error) {
	addr := net.JoinHostPort(c.profile.Host, strconv.Itoa(c.profile.Port))
	return dialFTPSession(addr, c.profile.Username, c.profile.Password, c.tlsConfig)
}

// Close disconnects every pooled connection. Safe to call multiple times.
func (c *ftpClient) Close() error {
	if c.pool != nil {
		c.pool.closeAll()
		c.pool = nil
	}
	return nil
}

// errBrokenSession marks an error after which a connection's session state
// can't be trusted (e.g. its working directory couldn't be restored), so it
// must not be reused even though the server is still answering.
var errBrokenSession = errors.New("ftp session state lost")

// sessionBroken reports whether a connection must be discarded after an
// operation returned err. A reply the server sent (a *textproto.Error such
// as 550 "no such file") leaves the session intact and the connection
// reusable — except 421, the server closing the connection. Local
// filesystem errors (*fs.PathError) say nothing about the connection.
// Anything else — a network error, a timeout, a garbled reply — may leave
// the command/response stream out of step, so the connection is dropped.
func sessionBroken(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, errBrokenSession) {
		return true
	}
	var tpErr *textproto.Error
	if errors.As(err, &tpErr) {
		return tpErr.Code == ftp.StatusNotAvailable
	}
	var pathErr *fs.PathError
	return !errors.As(err, &pathErr)
}

// withConn runs fn with a pooled connection held exclusively for its whole
// duration, under a deadline of timeout (fn may extend it via
// conn.begin), then returns the connection — or discards it if fn's error
// shows its session can't be trusted. A deadline expiring surfaces as a
// network timeout error, which discards the connection.
func (c *ftpClient) withConn(op string, timeout time.Duration, fn func(conn *ftpSession) error) error {
	if c.pool == nil {
		return wrapError(op, errPoolClosed)
	}
	conn, err := c.pool.acquire()
	if err != nil {
		return wrapError(op, err)
	}
	conn.begin(timeout)
	err = fn(conn)
	conn.end()
	c.pool.release(conn, sessionBroken(err))
	return err
}

// IsAlive checks an idle connection with NOOP. If all connections are busy
// they're in active use, so the client is considered alive; a dead one is
// discarded when its operation fails.
func (c *ftpClient) IsAlive() bool {
	if c.pool == nil {
		return false
	}
	conn, ok, busy := c.pool.tryAcquireIdle()
	if !ok {
		return busy
	}
	conn.begin(AliveTimeout) // a dead connection times out instead of hanging
	alive := conn.NoOp() == nil
	conn.end()
	c.pool.release(conn, !alive)
	return alive
}

// Stat returns info about a remote path
func (c *ftpClient) Stat(remotePath string) (*RemoteFileInfo, error) {
	var info *RemoteFileInfo
	err := c.withConn("stat", OperationTimeout, func(conn *ftpSession) error {
		entry, err := conn.GetEntry(remotePath)
		if err != nil {
			return wrapError("stat", err)
		}
		c.detectListOffset(conn, remotePath, entry)
		info = c.toRemote(conn, entry)
		return nil
	})
	return info, err
}

// ReadDir lists entries in a remote directory, skipping symlinks
func (c *ftpClient) ReadDir(remotePath string) ([]RemoteFileInfo, error) {
	var result []RemoteFileInfo
	err := c.withConn("readdir", ReadDirTimeout, func(conn *ftpSession) error {
		entries, err := conn.List(remotePath)
		if err != nil {
			return wrapError("readdir", err)
		}

		for _, entry := range entries {
			if c.detectListOffset(conn, RemotePath(remotePath, entry.Name), entry) {
				break
			}
		}

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
			result = append(result, *c.toRemote(conn, entry))
		}
		return nil
	})
	return result, err
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

	// Local errors surface before a connection is checked out.
	local, err := os.Open(localPath)
	if err != nil {
		return wrapError(op, err)
	}
	defer local.Close()

	localInfo, err := local.Stat()
	if err != nil {
		return wrapError(op, err)
	}

	return c.withConn(op, TransferTimeout(localInfo.Size()), func(conn *ftpSession) error {
		partial := remotePartialPath(remotePath)

		var offset int64
		if resume {
			size, sizeErr := conn.FileSize(partial)
			if sizeErr == nil {
				// No MDTM support means we can't tell whether the partial is
				// from the current version of the file — restart to be safe.
				if mtime, timeErr := conn.GetTime(partial); timeErr == nil &&
					canResumePartial(size, mtime, localInfo) {
					offset = size
				}
			}
		}

		var err error
		if offset > 0 {
			fmt.Fprintf(os.Stderr, "  resuming: %s (from %s, %s remaining)\n",
				remotePath, HumanSize(offset), HumanSize(localInfo.Size()-offset))
			if _, err = local.Seek(offset, io.SeekStart); err == nil {
				err = conn.StorFrom(partial, local, uint64(offset))
			}
			if err != nil {
				if sessionBroken(err) {
					return wrapError(op, err) // connection lost: let the caller retry
				}
				// REST unsupported or the partial is unusable — restart from zero
				fmt.Fprintf(os.Stderr, "  warning: resume failed (%s), doing full upload\n", translateError(err))
				offset = 0
			}
		}
		if offset == 0 {
			if _, err = local.Seek(0, io.SeekStart); err == nil {
				err = conn.Stor(partial, local)
			}
		}
		if err != nil {
			if !resume && !sessionBroken(err) {
				conn.Delete(partial)
			}
			return wrapError(op, err)
		}

		// Preserve mtime (MFMT, or vsftpd's writable MDTM) so the next diff
		// sees the file as in sync. Without it the remote mtime is the upload
		// time and every subsequent `sfync up` would re-upload every file.
		// Rename keeps the mtime, so set it on the partial before swapping it
		// in.
		if conn.IsSetTimeSupported() {
			if err := conn.SetTime(partial, localInfo.ModTime()); err != nil {
				fmt.Fprintf(os.Stderr, "  warning: set mtime failed for %s: %v\n", remotePath, err)
			}
		} else {
			warnNoSetTime(c.profile.Host)
		}

		if err := ftpReplace(conn, partial, remotePath); err != nil {
			if !sessionBroken(err) {
				conn.Delete(partial)
			}
			return wrapError(op, err)
		}

		fmt.Fprintf(os.Stderr, "  uploaded: %s (%s)\n", remotePath, HumanSize(localInfo.Size()))
		return nil
	})
}

// ftpReplace renames src over dst. Most Unix FTP servers overwrite on RNTO;
// servers that refuse (e.g. IIS) get dst deleted first, leaving only a brief
// window where dst is missing, never one where it is truncated.
func ftpReplace(conn *ftpSession, src, dst string) error {
	err := conn.Rename(src, dst)
	if err == nil || sessionBroken(err) {
		return err
	}
	conn.Delete(dst) // may fail if dst doesn't exist — the retry decides
	return conn.Rename(src, dst)
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
	return c.download(remotePath, localPath, nil)
}

// DownloadKnown is Download for a file whose size and mtime the caller
// already knows (e.g. from the mirror's remote scan), skipping the metadata
// lookup. See knownDownloader in mirror.go.
func (c *ftpClient) DownloadKnown(remotePath, localPath string, size int64, mtime time.Time) error {
	return c.download(remotePath, localPath, &RemoteFileInfo{Size: size, ModTime: mtime})
}

// download retrieves remotePath; known, if non-nil, supplies size and mtime
// (already timezone-corrected) so they needn't be looked up.
func (c *ftpClient) download(remotePath, localPath string, known *RemoteFileInfo) error {
	// Ensure local directory exists
	localDir := filepath.Dir(localPath)
	if err := os.MkdirAll(localDir, 0755); err != nil {
		return wrapError("download", err)
	}

	return c.withConn("download", OperationTimeout, func(conn *ftpSession) error {
		// Size sets the transfer's time budget; mtime is preserved locally.
		var mtime time.Time
		switch {
		case known != nil:
			mtime = known.ModTime
			conn.begin(TransferTimeout(known.Size))
		default:
			// Look both up (mtime corrected for the server's LIST timezone).
			if entry, statErr := conn.GetEntry(remotePath); statErr == nil {
				c.detectListOffset(conn, remotePath, entry)
				mtime = c.toRemote(conn, entry).ModTime
				conn.begin(TransferTimeout(int64(entry.Size)))
			} else if size, sizeErr := conn.FileSize(remotePath); sizeErr == nil {
				conn.begin(TransferTimeout(size))
			} else {
				conn.begin(unknownSizeTransferTimeout)
			}
		}

		// FTP Retr
		resp, err := conn.Retr(remotePath)
		if err != nil {
			return wrapError("download", err)
		}
		defer resp.Close() // idempotent; writeLocalAtomic closes it first

		// resp.Close is passed as finish: it reads the server's
		// end-of-transfer status, so a transfer the server aborted is never
		// committed as complete.
		n, err := writeLocalAtomic(localPath, resp, resp.Close, mtime)
		if err != nil {
			return wrapError("download", err)
		}

		fmt.Fprintf(os.Stderr, "  downloaded: %s (%s)\n", localPath, HumanSize(n))
		return nil
	})
}

// Mkdir creates a single remote directory whose parent already exists.
func (c *ftpClient) Mkdir(remotePath string) error {
	return c.withConn("mkdir", OperationTimeout, func(conn *ftpSession) error {
		if err := conn.MakeDir(remotePath); err != nil {
			return wrapError("mkdir", err)
		}
		return nil
	})
}

// MkdirAll creates a remote directory and all parents
func (c *ftpClient) MkdirAll(remotePath string) error {
	return c.withConn("mkdir", OperationTimeout, func(conn *ftpSession) error {
		// Fast path: the directory almost always exists already (the daemon
		// and push call MkdirAll before every upload). With MLST — exact
		// metadata for one path — a single command answers that; the
		// component walk below costs up to four commands per component.
		// (Without MLST, GetEntry falls back to LIST, which for a directory
		// lists its contents rather than describing it, so it can't be used.)
		if conn.IsTimePreciseInList() {
			if entry, err := conn.GetEntry(remotePath); err == nil && entry.Type == ftp.EntryTypeFolder {
				return nil
			} else if sessionBroken(err) && err != nil {
				return wrapError("mkdir", err)
			}
		}

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

			// Each component gets its own budget, so a deep path can't
			// exhaust a single shared one.
			conn.begin(OperationTimeout)

			// Try to create — if it already exists, the error is expected
			err := conn.MakeDir(current)
			if err != nil {
				if sessionBroken(err) {
					return wrapError("mkdir", err)
				}
				// FTP 550 can mean "already exists" OR "permission denied".
				// Use ChangeDir to check existence — O(1) vs a full List call.
				cwd, cwdErr := conn.CurrentDir()
				if cwdErr == nil {
					cdErr := conn.ChangeDir(current)
					// Always restore cwd. If that fails the session's working
					// directory is wrong, so the connection must not be
					// reused for anything else (errBrokenSession).
					if restoreErr := conn.ChangeDir(cwd); restoreErr != nil {
						return wrapError("mkdir", fmt.Errorf("failed to restore working directory after checking %s: %w",
							current, errors.Join(errBrokenSession, restoreErr)))
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
	})
}

// Remove deletes a remote file
func (c *ftpClient) Remove(remotePath string) error {
	return c.withConn("remove", OperationTimeout, func(conn *ftpSession) error {
		if err := conn.Delete(remotePath); err != nil {
			return wrapError("remove", err)
		}
		return nil
	})
}

// RemoveDir deletes an empty remote directory
func (c *ftpClient) RemoveDir(remotePath string) error {
	return c.withConn("rmdir", OperationTimeout, func(conn *ftpSession) error {
		if err := conn.RemoveDir(remotePath); err != nil {
			return wrapError("rmdir", err)
		}
		return nil
	})
}

// Chmod is a no-op for FTP (no standard permission model)
func (c *ftpClient) Chmod(_ string, _ os.FileMode) error {
	return nil
}

// toRemote converts an ftp.Entry to RemoteFileInfo, correcting LIST times
// for the server's timezone.
func (c *ftpClient) toRemote(conn *ftpSession, entry *ftp.Entry) *RemoteFileInfo {
	precise := conn.IsTimePreciseInList()
	mtime := entry.Time
	if !precise {
		c.offsetMu.Lock()
		mtime = mtime.Add(-c.listOffset)
		c.offsetMu.Unlock()
	}
	return &RemoteFileInfo{
		Name:             entry.Name,
		Size:             int64(entry.Size),
		ModTime:          mtime,
		IsDir:            entry.Type == ftp.EntryTypeFolder,
		ModTimePrecision: listTimePrecision(mtime, precise, time.Now()),
	}
}

// maxListOffsetTries bounds MDTM probes when they keep failing, so a server
// with a broken MDTM costs at most a few extra round trips per client.
const maxListOffsetTries = 3

// detectListOffset works out the server's LIST timezone offset from one
// file, reporting whether the offset is now known. LIST times have no
// timezone and many servers emit local time, while MDTM is defined to be
// UTC (RFC 3659) — so for a recent file (listed to the minute), the LIST
// time minus its MDTM time is the server's offset, give or take the
// truncated seconds. Rounded to 15 minutes, the granularity of real
// timezones. Costs one MDTM round trip per client. Without detection, a
// server listing in UTC-7 would make every file look 7 hours off and get
// re-uploaded on every sync.
//
// Holds offsetMu throughout, so concurrent listings neither probe twice nor
// read the offset mid-detection.
func (c *ftpClient) detectListOffset(conn *ftpSession, remotePath string, entry *ftp.Entry) bool {
	c.offsetMu.Lock()
	defer c.offsetMu.Unlock()
	if c.listOffsetKnown {
		return true
	}
	if conn.IsTimePreciseInList() || !conn.IsGetTimeSupported() {
		c.listOffsetKnown = true // nothing to correct, or no way to tell
		return true
	}
	// Need a file listed with minute precision (a recent one): date-only
	// entries for older files can't pin the offset down. Its listed time may
	// be skewed by up to ±14h before correction, hence the widened "now".
	if entry.Type != ftp.EntryTypeFile || c.listOffsetTries >= maxListOffsetTries ||
		listTimePrecision(entry.Time, false, time.Now().Add(14*time.Hour)) != time.Minute {
		return false
	}
	c.listOffsetTries++
	mdtm, err := conn.GetTime(remotePath)
	if err != nil {
		return false
	}
	offset, ok := listOffsetFrom(entry.Time, mdtm)
	if !ok {
		return false // implausible; MDTM or LIST is lying
	}
	c.listOffset = offset
	c.listOffsetKnown = true
	if offset != 0 {
		fmt.Fprintf(os.Stderr, "  note: %s lists file times in UTC%+gh (detected); adjusting\n",
			c.profile.Host, offset.Hours())
	}
	return true
}

// listTimePrecision is the granularity of an entry's timestamp. MLSD/MLST
// times are exact. Unix-style LIST shows "Jan _2 15:04" (minute precision)
// for files modified within the last six months and "Jan _2  2006" (date
// only, parsed as midnight) for older ones. The six-month cutoff is decided
// by the server's clock, so anything older than five months is treated as
// date-only — slightly looser for files 5-6 months old, never wrong.
func listTimePrecision(t time.Time, preciseTimes bool, now time.Time) time.Duration {
	switch {
	case preciseTimes:
		return 0
	case now.Sub(t) > 150*24*time.Hour:
		return 24 * time.Hour
	default:
		return time.Minute
	}
}

// listOffsetFrom derives a server's LIST timezone offset from one file's
// LIST time (local wall clock, truncated to the minute, parsed as if UTC)
// and its MDTM time (true UTC). The truncation makes the raw difference up
// to 59s short of the true offset; rounding to 15 minutes — the granularity
// of real timezones (e.g. UTC+5:30, UTC+5:45) — removes it.
func listOffsetFrom(listed, mdtm time.Time) (time.Duration, bool) {
	offset := listed.Sub(mdtm).Round(15 * time.Minute)
	if offset < -14*time.Hour || offset > 14*time.Hour {
		return 0, false
	}
	return offset, true
}
