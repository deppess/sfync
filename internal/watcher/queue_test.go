package watcher

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/deppess/sfync/internal/config"
	"github.com/deppess/sfync/internal/transfer"
)

// A file that disappears before its debounce fires (an editor's atomic-save
// temp file) must be dropped silently — no retries, no error callback.
// The pool has no reachable server, so reaching the network would fail the
// test via onError.
func TestProcessUploadSkipsVanishedAndPartialFiles(t *testing.T) {
	ctx := t.TempDir()
	profiles := map[string]*config.Profile{
		"p": {Host: "127.0.0.1", Port: 1, Context: ctx, RemotePath: "/r"},
	}
	q := NewUploadQueue(profiles, transfer.NewConnectionPool(profiles))

	partial := filepath.Join(ctx, ".app.js.42.sfync-part")
	if err := os.WriteFile(partial, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	for _, file := range []string{
		filepath.Join(ctx, "4913"), // never existed / already deleted
		partial,                    // sfync's own in-progress file
	} {
		q.processUpload(&uploadTask{profileName: "p", filePath: file},
			func(_, rel string) { t.Errorf("onSuccess called for %s", rel) },
			func(_, rel string, err error, _ int) { t.Errorf("onError called for %s: %v", rel, err) },
		)
	}
}

// fakeRemote is a RemoteClient double: remote holds which remote paths
// exist (true = directory) and every mutating call is recorded.
type fakeRemote struct {
	transfer.RemoteClient // unimplemented methods panic if reached
	remote                map[string]bool
	dirNotEmpty           bool
	calls                 []string
}

func (f *fakeRemote) Stat(p string) (*transfer.RemoteFileInfo, error) {
	isDir, ok := f.remote[p]
	if !ok {
		return nil, fmt.Errorf("stat: %w", fs.ErrNotExist)
	}
	return &transfer.RemoteFileInfo{IsDir: isDir}, nil
}
func (f *fakeRemote) Remove(p string) error { f.calls = append(f.calls, "rm "+p); return nil }
func (f *fakeRemote) RemoveDir(p string) error {
	f.calls = append(f.calls, "rmdir "+p)
	if f.dirNotEmpty {
		return errors.New("directory not empty")
	}
	return nil
}
func (f *fakeRemote) MkdirAll(p string) error { return nil }
func (f *fakeRemote) Upload(l, r string) error {
	f.calls = append(f.calls, "upload "+r)
	return nil
}

type fakeSource struct {
	client *fakeRemote
	gets   int
}

func (s *fakeSource) Get(string) (transfer.RemoteClient, error) { s.gets++; return s.client, nil }

type result struct{ successes, errs []string }

func runTask(t *testing.T, profile *config.Profile, src *fakeSource, file string) result {
	t.Helper()
	q := NewUploadQueue(map[string]*config.Profile{"p": profile}, src)
	var r result
	q.processUpload(&uploadTask{profileName: "p", filePath: file},
		func(_, rel string) { r.successes = append(r.successes, rel) },
		func(_, rel string, err error, _ int) { r.errs = append(r.errs, rel+": "+err.Error()) },
	)
	return r
}

func TestDeletionDisabledByDefault(t *testing.T) {
	ctx := t.TempDir()
	src := &fakeSource{client: &fakeRemote{remote: map[string]bool{"/r/gone.html": false}}}
	r := runTask(t, &config.Profile{Context: ctx, RemotePath: "/r"}, src, filepath.Join(ctx, "gone.html"))

	if src.gets != 0 || len(src.client.calls) != 0 || len(r.successes)+len(r.errs) != 0 {
		t.Errorf("deletes disabled must not touch the remote: gets=%d calls=%v result=%+v", src.gets, src.client.calls, r)
	}
}

func TestDeletionRemovesRemoteFile(t *testing.T) {
	ctx := t.TempDir()
	src := &fakeSource{client: &fakeRemote{remote: map[string]bool{"/r/sub/gone.html": false}}}
	p := &config.Profile{Context: ctx, RemotePath: "/r", AutoSyncDeletes: true}
	r := runTask(t, p, src, filepath.Join(ctx, "sub", "gone.html"))

	if got := strings.Join(src.client.calls, ","); got != "rm /r/sub/gone.html" {
		t.Errorf("calls = %q", got)
	}
	if len(r.successes) != 1 || r.successes[0] != "sub/gone.html (deleted)" || len(r.errs) != 0 {
		t.Errorf("result = %+v", r)
	}
}

func TestDeletionNoOpWhenRemoteAlreadyAbsent(t *testing.T) {
	ctx := t.TempDir()
	src := &fakeSource{client: &fakeRemote{remote: map[string]bool{}}}
	p := &config.Profile{Context: ctx, RemotePath: "/r", AutoSyncDeletes: true}
	r := runTask(t, p, src, filepath.Join(ctx, "never-uploaded.tmp"))

	if len(src.client.calls) != 0 || len(r.successes)+len(r.errs) != 0 {
		t.Errorf("calls=%v result=%+v", src.client.calls, r)
	}
}

func TestDeletionSkippedWhenContextMissing(t *testing.T) {
	ctx := filepath.Join(t.TempDir(), "unmounted-drive")
	src := &fakeSource{client: &fakeRemote{remote: map[string]bool{"/r/a.html": false}}}
	p := &config.Profile{Context: ctx, RemotePath: "/r", AutoSyncDeletes: true}
	r := runTask(t, p, src, filepath.Join(ctx, "a.html"))

	if src.gets != 0 || len(src.client.calls) != 0 || len(r.successes)+len(r.errs) != 0 {
		t.Errorf("missing context must never trigger deletes: gets=%d calls=%v", src.gets, src.client.calls)
	}
}

func TestDeletionRespectsSyncignore(t *testing.T) {
	ctx := t.TempDir()
	os.WriteFile(filepath.Join(ctx, ".syncignore"), []byte("*.log\n"), 0o644)
	src := &fakeSource{client: &fakeRemote{remote: map[string]bool{"/r/debug.log": false}}}
	p := &config.Profile{Context: ctx, RemotePath: "/r", AutoSyncDeletes: true}
	runTask(t, p, src, filepath.Join(ctx, "debug.log"))

	if len(src.client.calls) != 0 {
		t.Errorf("ignored file deleted remotely: %v", src.client.calls)
	}
}

func TestDeletionKeepsNonEmptyRemoteDirWithoutError(t *testing.T) {
	ctx := t.TempDir()
	src := &fakeSource{client: &fakeRemote{remote: map[string]bool{"/r/assets": true}, dirNotEmpty: true}}
	p := &config.Profile{Context: ctx, RemotePath: "/r", AutoSyncDeletes: true}
	r := runTask(t, p, src, filepath.Join(ctx, "assets"))

	if got := strings.Join(src.client.calls, ","); got != "rmdir /r/assets" {
		t.Errorf("calls = %q (must be a single non-recursive rmdir)", got)
	}
	if len(r.errs) != 0 || len(r.successes) != 0 {
		t.Errorf("non-empty dir should be kept quietly: %+v", r)
	}
}

func TestPresentFileIsUploadedEvenWithDeletesOn(t *testing.T) {
	ctx := t.TempDir()
	file := filepath.Join(ctx, "index.html")
	os.WriteFile(file, []byte("hi"), 0o644)
	src := &fakeSource{client: &fakeRemote{remote: map[string]bool{}}}
	p := &config.Profile{Context: ctx, RemotePath: "/r", AutoSyncDeletes: true}
	r := runTask(t, p, src, file)

	if got := strings.Join(src.client.calls, ","); got != "upload /r/index.html" {
		t.Errorf("calls = %q", got)
	}
	if len(r.successes) != 1 || r.successes[0] != "index.html" {
		t.Errorf("result = %+v", r)
	}
}
