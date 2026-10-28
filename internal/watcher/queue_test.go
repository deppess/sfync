package watcher

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

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
	mkdirs                int // MkdirAll calls
	failUploads           int // upload attempts to fail before succeeding
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
func (f *fakeRemote) MkdirAll(p string) error { f.mkdirs++; return nil }
func (f *fakeRemote) Upload(l, r string) error {
	f.calls = append(f.calls, "upload "+r)
	if f.failUploads > 0 {
		f.failUploads--
		return errors.New("connection reset")
	}
	return nil
}

type fakeSource struct {
	client *fakeRemote
	gets   int
}

func (s *fakeSource) Acquire(string) (transfer.RemoteClient, func(error), error) {
	s.gets++
	return s.client, func(error) {}, nil
}

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

	// Retried (concurrent file deletes may still be emptying it), never
	// recursive, and finally kept quietly — no error notification.
	if got := strings.Join(src.client.calls, ","); got != "rmdir /r/assets,rmdir /r/assets,rmdir /r/assets" {
		t.Errorf("calls = %q (want only non-recursive rmdir attempts)", got)
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

// Repeated uploads into the same remote directory ensure it only once; a
// failed upload forgets it so the retry checks again.
func TestQueueRemembersEnsuredDirectories(t *testing.T) {
	ctx := t.TempDir()
	os.MkdirAll(filepath.Join(ctx, "css"), 0o755)
	for _, f := range []string{"css/a.css", "css/b.css", "css/c.css"} {
		os.WriteFile(filepath.Join(ctx, f), []byte("x"), 0o644)
	}
	src := &fakeSource{client: &fakeRemote{remote: map[string]bool{}}}
	q := NewUploadQueue(map[string]*config.Profile{"p": {Context: ctx, RemotePath: "/r"}}, src)
	ok := func(string, string) {}
	fail := func(_, rel string, err error, _ int) { t.Errorf("upload %s failed: %v", rel, err) }

	for _, f := range []string{"css/a.css", "css/b.css", "css/c.css"} {
		q.processUpload(&uploadTask{profileName: "p", filePath: filepath.Join(ctx, f)}, ok, fail)
	}
	if m := src.client.mkdirs; m != 1 {
		t.Errorf("MkdirAll called %d times for 3 uploads into one directory, want 1", m)
	}

	// A failed upload (e.g. the directory vanished remotely) forgets it:
	// the retry must ensure the directory again.
	src.client.failUploads = 1
	q.processUpload(&uploadTask{profileName: "p", filePath: filepath.Join(ctx, "css/a.css")}, ok, fail)
	if m := src.client.mkdirs; m != 2 {
		t.Errorf("after a failed upload, MkdirAll calls = %d, want 2 (re-ensured on retry)", m)
	}

	// A config reload forgets everything.
	q.UpdateProfiles(map[string]*config.Profile{"p": {Context: ctx, RemotePath: "/r"}})
	q.processUpload(&uploadTask{profileName: "p", filePath: filepath.Join(ctx, "css/b.css")}, ok, fail)
	if m := src.client.mkdirs; m != 3 {
		t.Errorf("after reload, MkdirAll calls = %d, want 3", m)
	}
}

// blockingRemote's uploads wait on a gate, tracking concurrency per path.
type blockingRemote struct {
	transfer.RemoteClient
	gate    chan struct{}
	mu      sync.Mutex
	active  map[string]int
	maxSame int // most concurrent uploads of one path
	maxAll  int // most concurrent uploads overall
	count   map[string]int
	all     int
}

func (b *blockingRemote) MkdirAll(string) error { return nil }
func (b *blockingRemote) Upload(_, remote string) error {
	b.mu.Lock()
	b.active[remote]++
	b.all++
	b.maxSame = max(b.maxSame, b.active[remote])
	b.maxAll = max(b.maxAll, b.all)
	b.count[remote]++
	b.mu.Unlock()
	<-b.gate
	b.mu.Lock()
	b.active[remote]--
	b.all--
	b.mu.Unlock()
	return nil
}

type blockingSource struct{ c *blockingRemote }

func (s blockingSource) Acquire(string) (transfer.RemoteClient, func(error), error) {
	return s.c, func(error) {}, nil
}

// Different files upload in parallel; one file is never uploaded twice at
// once, and a change that arrives during its upload is synced afterwards.
func TestQueueConcurrencyAndPerFileExclusivity(t *testing.T) {
	ctx := t.TempDir()
	var files []string
	for i := range 6 {
		f := filepath.Join(ctx, fmt.Sprintf("f%d.txt", i))
		os.WriteFile(f, []byte("x"), 0o644)
		files = append(files, f)
	}
	remote := &blockingRemote{gate: make(chan struct{}), active: map[string]int{}, count: map[string]int{}}
	q := NewUploadQueue(map[string]*config.Profile{"p": {Context: ctx, RemotePath: "/r"}}, blockingSource{remote})
	var done sync.WaitGroup
	q.Start(func(_, _ string) { done.Done() }, func(_, rel string, err error, _ int) {
		t.Errorf("%s failed: %v", rel, err)
		done.Done()
	})
	defer q.Stop()

	waitFor := func(cond func() bool) {
		t.Helper()
		deadline := time.Now().Add(5 * time.Second)
		for !cond() {
			if time.Now().After(deadline) {
				t.Fatal("timed out")
			}
			time.Sleep(5 * time.Millisecond)
		}
	}
	inFlight := func() int { remote.mu.Lock(); defer remote.mu.Unlock(); return remote.all }

	// Six different files: all upload at once (within queueWorkers).
	done.Add(len(files))
	for _, f := range files {
		q.Enqueue("p", f)
	}
	waitFor(func() bool { return inFlight() == len(files) })

	// f0 changes twice more while its upload is in flight: must queue, not
	// run concurrently — and the two changes collapse into one re-upload.
	done.Add(1)
	q.Enqueue("p", files[0])
	q.Enqueue("p", files[0])
	time.Sleep(50 * time.Millisecond)

	close(remote.gate) // let everything finish
	finished := make(chan struct{})
	go func() { done.Wait(); close(finished) }()
	select {
	case <-finished:
	case <-time.After(5 * time.Second):
		t.Fatal("uploads never completed")
	}

	remote.mu.Lock()
	defer remote.mu.Unlock()
	if remote.maxSame != 1 {
		t.Errorf("a file was uploaded %d times concurrently", remote.maxSame)
	}
	if remote.maxAll < len(files) {
		t.Errorf("max concurrent uploads = %d, want %d (parallel)", remote.maxAll, len(files))
	}
	if n := remote.count["/r/f0.txt"]; n != 2 {
		t.Errorf("f0 uploaded %d times, want 2 (original + one collapsed re-sync)", n)
	}
}
