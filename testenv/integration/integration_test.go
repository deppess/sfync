// Package integration runs sfync's sync engine end to end against every
// test server kind (real OpenSSH, in-process SFTP, in-process FTP), so
// behavior that depends on the server — rename semantics, mtime support,
// listing formats — is verified against each.
package integration

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/deppess/sfync/internal/transfer"
	"github.com/deppess/sfync/testenv/fixtures"
	"github.com/deppess/sfync/testenv/netem"
	"github.com/deppess/sfync/testenv/servers"
)

var kinds = []struct {
	name  string
	start servers.Starter
}{
	{"openssh", servers.StartOpenSSH},
	{"sftp-inproc", servers.StartInProcSFTP},
	{"ftp", servers.StartFTP},
}

// small is a quick tree with nesting, used for correctness (not speed).
var small = fixtures.Spec{Name: "it-small", Dirs: 8, Depth: 3, Files: 40, MinSize: 100, MaxSize: 64 << 10}

// localCopy returns a mutable copy of the small fixture tree.
func localCopy(t *testing.T) string {
	t.Helper()
	src, _, err := fixtures.Get(small)
	if err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(t.TempDir(), "local")
	if err := fixtures.Copy(src, dst); err != nil {
		t.Fatal(err)
	}
	return dst
}

func connect(t *testing.T, srv *servers.Server, local string) (transfer.RemoteClient, func() *transfer.MirrorResult) {
	t.Helper()
	profile := srv.Profile(local)
	c := transfer.NewClient(profile)
	if err := c.Connect(); err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() { c.Close() })
	up := func() *transfer.MirrorResult {
		t.Helper()
		res, err := transfer.MirrorUp(c, profile, false)
		if err != nil {
			t.Fatalf("mirror up: %v", err)
		}
		if !res.Success {
			t.Fatalf("mirror up errors: %v", res.Errors)
		}
		return res
	}
	return c, up
}

// treesEqual compares two local trees by relative path and content.
func treesEqual(t *testing.T, a, b string) {
	t.Helper()
	read := func(root string) map[string]string {
		m := map[string]string{}
		filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() {
				return err
			}
			rel, _ := filepath.Rel(root, p)
			data, _ := os.ReadFile(p)
			m[rel] = string(data)
			return nil
		})
		return m
	}
	ma, mb := read(a), read(b)
	if len(ma) != len(mb) {
		t.Errorf("file count %d vs %d", len(ma), len(mb))
	}
	for k, v := range ma {
		if mb[k] != v {
			t.Errorf("%s differs or missing", k)
		}
	}
}

func noPartials(t *testing.T, root string) {
	t.Helper()
	filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err == nil && transfer.IsPartialName(d.Name()) {
			t.Errorf("leftover partial file: %s", p)
		}
		return nil
	})
}

func TestMirrorUpThenNoop(t *testing.T) {
	for _, k := range kinds {
		t.Run(k.name, func(t *testing.T) {
			srv := k.start(t, netem.Local)
			local := localCopy(t)
			_, up := connect(t, srv, local)

			first := up()
			if first.Uploaded != small.Files {
				t.Errorf("uploaded %d, want %d", first.Uploaded, small.Files)
			}
			treesEqual(t, local, srv.Root)
			noPartials(t, srv.Root)

			// A second sync of an unchanged tree must transfer nothing — this
			// is what mtime preservation (SFTP chtimes, FTP MFMT) buys.
			second := up()
			if second.Uploaded != 0 || second.Deleted != 0 || second.Created != 0 {
				t.Errorf("second run not a no-op: uploaded=%d deleted=%d created=%d",
					second.Uploaded, second.Deleted, second.Created)
			}
		})
	}
}

func TestMirrorDownRoundTrip(t *testing.T) {
	for _, k := range kinds {
		t.Run(k.name, func(t *testing.T) {
			srv := k.start(t, netem.Local)
			local := localCopy(t)
			_, up := connect(t, srv, local)
			up()

			fresh := filepath.Join(t.TempDir(), "fresh")
			os.Mkdir(fresh, 0o755)
			c2, _ := connect(t, srv, fresh)
			res, err := transfer.MirrorDown(c2, srv.Profile(fresh), false)
			if err != nil || !res.Success {
				t.Fatalf("mirror down: %v %v", err, res.Errors)
			}
			treesEqual(t, local, fresh)
			noPartials(t, fresh)
		})
	}
}

func TestMirrorUpPropagatesChangesAndDeletes(t *testing.T) {
	for _, k := range kinds {
		t.Run(k.name, func(t *testing.T) {
			srv := k.start(t, netem.Local)
			local := localCopy(t)
			_, up := connect(t, srv, local)
			up()

			// Change one file, delete one file and one whole directory.
			var files, dirs []string
			filepath.WalkDir(local, func(p string, d fs.DirEntry, err error) error {
				if p == local {
					return nil
				}
				if d.IsDir() {
					dirs = append(dirs, p)
				} else {
					files = append(files, p)
				}
				return nil
			})
			os.WriteFile(files[0], []byte("changed"), 0o644)
			os.Remove(files[1])
			os.RemoveAll(dirs[len(dirs)-1])

			res := up()
			if res.Uploaded != 1 {
				t.Errorf("uploaded %d, want 1", res.Uploaded)
			}
			if res.Deleted == 0 {
				t.Error("nothing deleted remotely")
			}
			treesEqual(t, local, srv.Root)
		})
	}
}

func TestFailedUploadKeepsLiveFile(t *testing.T) {
	for _, k := range kinds {
		t.Run(k.name, func(t *testing.T) {
			srv := k.start(t, netem.Local)
			c, _ := connect(t, srv, t.TempDir())

			os.WriteFile(filepath.Join(srv.Root, "live.html"), []byte("live"), 0o644)
			err := c.Upload(filepath.Join(t.TempDir(), "missing"), transfer.RemotePath(srv.RemoteRoot, "live.html"))
			if err == nil {
				t.Fatal("expected error")
			}
			if got, _ := os.ReadFile(filepath.Join(srv.Root, "live.html")); string(got) != "live" {
				t.Errorf("live file changed to %q", got)
			}
			noPartials(t, srv.Root)
		})
	}
}

func TestUploadResumeContinuesPartial(t *testing.T) {
	for _, k := range kinds {
		t.Run(k.name, func(t *testing.T) {
			srv := k.start(t, netem.Local)
			c, _ := connect(t, srv, t.TempDir())

			src := filepath.Join(t.TempDir(), "big.bin")
			os.WriteFile(src, []byte(strings.Repeat("0123456789", 1000)), 0o644)
			past := time.Now().Add(-time.Hour)
			os.Chtimes(src, past, past)

			// A previous attempt left 5000 bytes; mark byte 4999 so we can
			// tell the partial was continued rather than replaced.
			partial := []byte(strings.Repeat("0123456789", 500))
			partial[4999] = 'X'
			os.WriteFile(filepath.Join(srv.Root, ".big.bin.sfync-part"), partial, 0o644)

			if err := c.UploadResume(src, transfer.RemotePath(srv.RemoteRoot, "big.bin")); err != nil {
				t.Fatal(err)
			}
			got, _ := os.ReadFile(filepath.Join(srv.Root, "big.bin"))
			if len(got) != 10000 || got[4999] != 'X' || got[5000] != '0' {
				t.Errorf("partial not continued: len=%d byte4999=%q", len(got), got[min(4999, len(got)-1)])
			}
			noPartials(t, srv.Root)
		})
	}
}

// Servers without MFMT can't have their mtimes set, so sfync can't tell an
// unchanged file from a changed one by time — documented behavior: the
// second run re-uploads. This pins it down so a change is noticed.
func TestFTPWithoutMFMTReuploads(t *testing.T) {
	srv := servers.StartFTPWith(t, netem.Local, servers.FTPOptions{DisableMFMT: true})
	local := localCopy(t)
	_, up := connect(t, srv, local)
	up()
	if second := up(); second.Uploaded == 0 {
		t.Error("expected re-uploads without MFMT; if this now passes, sfync gained another way to preserve sync state — update this test")
	}
}

// LIST-only servers (no MLSD) report coarse timestamps. Files whose mtimes
// aren't on a whole minute must still be seen as in sync after an upload.
func TestFTPWithoutMLSDNoop(t *testing.T) {
	srv := servers.StartFTPWith(t, netem.Local, servers.FTPOptions{DisableMLSD: true})
	local := localCopy(t)
	odd := time.Now().Add(-48 * time.Hour).Truncate(time.Second).Add(37 * time.Second)
	filepath.WalkDir(local, func(p string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			os.Chtimes(p, odd, odd)
		}
		return nil
	})
	c, up := connect(t, srv, local)
	up()
	if second := up(); second.Uploaded != 0 {
		t.Errorf("LIST-only server: second run re-uploaded %d files (timestamp precision)", second.Uploaded)
		// Show what the listing reported for one file, to diagnose.
		entries, err := c.ReadDir(srv.RemoteRoot)
		for _, e := range entries {
			if !e.IsDir {
				t.Logf("local mtime %v; listed %v (precision %v), offset %v",
					odd.UTC(), e.ModTime.UTC(), e.ModTimePrecision, odd.Sub(e.ModTime))
				break
			}
		}
		if err != nil {
			t.Log(err)
		}
	}
}

// Uploads preserve permission bits on SFTP servers — chmod now runs
// concurrently with closing the handle, so check it on each server kind.
// (FTP has no standard permission model; skipped.)
func TestUploadPreservesMode(t *testing.T) {
	for _, k := range kinds {
		if k.name == "ftp" {
			continue
		}
		t.Run(k.name, func(t *testing.T) {
			srv := k.start(t, netem.Local)
			local := filepath.Join(t.TempDir(), "local")
			os.Mkdir(local, 0o755)
			modes := map[string]os.FileMode{"script.sh": 0o755, "private.txt": 0o600, "page.html": 0o644, "shared.txt": 0o664}
			for name, mode := range modes {
				p := filepath.Join(local, name)
				os.WriteFile(p, []byte(name), 0o600)
				os.Chmod(p, mode)
			}
			_, up := connect(t, srv, local)
			up()
			for name, want := range modes {
				fi, err := os.Stat(filepath.Join(srv.Root, name))
				if err != nil {
					t.Fatal(err)
				}
				if got := fi.Mode().Perm(); got != want {
					t.Errorf("%s: remote mode %04o, want %04o", name, got, want)
				}
			}
		})
	}
}
