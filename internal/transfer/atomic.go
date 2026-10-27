package transfer

import (
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"
)

// partialSuffix marks in-progress transfer files. Both directions write to a
// partial file next to the destination and rename it into place only once
// the transfer has fully succeeded, so a failed or interrupted transfer never
// leaves the destination truncated (a half-uploaded file on a live server) or
// deleted (a failed re-download destroying the good local copy).
//
// Files carrying this suffix are skipped by both tree walks and by the
// auto-sync watcher, so they are never synced, diffed, or mirror-deleted.
const partialSuffix = ".sfync-part"

// IsPartialName reports whether a file name is an sfync in-progress
// transfer file (see partialSuffix).
func IsPartialName(name string) bool {
	return strings.HasSuffix(name, partialSuffix)
}

// remotePartialPath returns the fixed partial-upload path for remotePath:
// a hidden sibling, e.g. /www/app.js → /www/.app.js.sfync-part. The name is
// deterministic (unlike the local temp file) so UploadResume can find a
// previous attempt's partial and continue it.
func remotePartialPath(remotePath string) string {
	dir, base := path.Split(remotePath)
	return dir + "." + base + partialSuffix
}

// canResumePartial reports whether a remote partial upload left behind by a
// previous attempt can be continued rather than restarted. It must be no
// larger than the local file, and the local file must not have been modified
// since the partial was last written — otherwise the partial's bytes are a
// prefix of an older version and appending to them would corrupt the file.
func canResumePartial(partialSize int64, partialModTime time.Time, local os.FileInfo) bool {
	return partialSize > 0 &&
		partialSize <= local.Size() &&
		!local.ModTime().After(partialModTime)
}

// writeLocalAtomic streams src into a temporary file beside dst, then renames
// it over dst. dst is only ever replaced by a complete file; on any failure
// the temp file is removed and an existing dst is left untouched.
//
// finish, if non-nil, is called exactly once after the copy (whether or not
// the copy failed) and its error fails the download — FTP uses it to confirm
// the server reported a complete transfer before the file is committed.
//
// The rename also replaces a symlink at dst rather than following it, so a
// symlink planted in the sync directory can't redirect a download elsewhere.
// A non-zero mtime is applied to the file (non-fatal on failure). An existing
// regular file's permission bits are preserved; new files get 0644.
func writeLocalAtomic(dst string, src io.Reader, finish func() error, mtime time.Time) (int64, error) {
	tmp, err := os.CreateTemp(filepath.Dir(dst), "."+filepath.Base(dst)+".*"+partialSuffix)
	if err != nil {
		if finish != nil {
			finish()
		}
		return 0, err
	}
	tmpName := tmp.Name()
	committed := false
	defer func() {
		if !committed {
			os.Remove(tmpName)
		}
	}()

	n, err := io.Copy(tmp, src)
	if finish != nil {
		if ferr := finish(); err == nil {
			err = ferr
		}
	}
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return n, err
	}

	mode := os.FileMode(0o644)
	if fi, lerr := os.Lstat(dst); lerr == nil && fi.Mode().IsRegular() {
		mode = fi.Mode().Perm()
	}
	if err := os.Chmod(tmpName, mode); err != nil {
		return n, err
	}
	if !mtime.IsZero() {
		if err := os.Chtimes(tmpName, mtime, mtime); err != nil {
			fmt.Fprintf(os.Stderr, "  warning: chtimes failed for %s: %v\n", dst, err)
		}
	}

	if err := os.Rename(tmpName, dst); err != nil {
		return n, err
	}
	committed = true
	return n, nil
}
