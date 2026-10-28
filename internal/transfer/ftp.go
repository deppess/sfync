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

	// LIST timestamps carry no timezone; many servers list in their local
	// time. listOffset is how far the server's LIST times are ahead of UTC,
	// detected once per connection (see detectListOffset).
	listOffset      time.Duration
	listOffsetKnown bool
	listOffsetTries int
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
	c.listOffset, c.listOffsetKnown, c.listOffsetTries = 0, false, 0
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
	c.detectListOffset(remotePath, entry)
	return c.toRemote(entry), nil
}

// ReadDir lists entries in a remote directory, skipping symlinks
func (c *ftpClient) ReadDir(remotePath string) ([]RemoteFileInfo, error) {
	entries, err := c.conn.List(remotePath)
	if err != nil {
		return nil, wrapError("readdir", err)
	}

	if !c.listOffsetKnown {
		for _, entry := range entries {
			if c.detectListOffset(RemotePath(remotePath, entry.Name), entry) {
				break
			}
		}
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
		result = append(result, *c.toRemote(entry))
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

// Mkdir creates a single remote directory whose parent already exists.
func (c *ftpClient) Mkdir(remotePath string) error {
	if err := c.conn.MakeDir(remotePath); err != nil {
		return wrapError("mkdir", err)
	}
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
func (c *ftpClient) toRemote(entry *ftp.Entry) *RemoteFileInfo {
	precise := c.conn.IsTimePreciseInList()
	mtime := entry.Time
	if !precise {
		mtime = mtime.Add(-c.listOffset)
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
// with a broken MDTM costs at most a few extra round trips per connection.
const maxListOffsetTries = 3

// detectListOffset works out the server's LIST timezone offset from one
// file, reporting whether the offset is now known. LIST times have no
// timezone and many servers emit local time, while MDTM is defined to be
// UTC (RFC 3659) — so for a recent file (listed to the minute), the LIST
// time minus its MDTM time is the server's offset, give or take the
// truncated seconds. Rounded to 15 minutes, the granularity of real
// timezones. Costs one MDTM round trip per connection. Without detection,
// a server listing in UTC-7 would make every file look 7 hours off and
// get re-uploaded on every sync.
func (c *ftpClient) detectListOffset(remotePath string, entry *ftp.Entry) bool {
	if c.listOffsetKnown {
		return true
	}
	if c.conn.IsTimePreciseInList() || !c.conn.IsGetTimeSupported() {
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
	mdtm, err := c.conn.GetTime(remotePath)
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
