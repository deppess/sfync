package transfer

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// failingReader yields some data, then an error — a transfer that dies midway.
type failingReader struct{ sent bool }

func (r *failingReader) Read(p []byte) (int, error) {
	if !r.sent {
		r.sent = true
		return copy(p, "partial-data"), nil
	}
	return 0, errors.New("connection reset")
}

func listDir(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	return names
}

func TestWriteLocalAtomicReplacesFile(t *testing.T) {
	dir := t.TempDir()
	dst := filepath.Join(dir, "index.html")
	if err := os.WriteFile(dst, []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}

	mtime := time.Date(2024, 1, 2, 3, 4, 5, 0, time.UTC)
	n, err := writeLocalAtomic(dst, strings.NewReader("new contents"), nil, mtime)
	if err != nil {
		t.Fatalf("writeLocalAtomic: %v", err)
	}
	if n != int64(len("new contents")) {
		t.Errorf("n = %d", n)
	}

	got, _ := os.ReadFile(dst)
	if string(got) != "new contents" {
		t.Errorf("contents = %q", got)
	}
	fi, _ := os.Stat(dst)
	if !fi.ModTime().Equal(mtime) {
		t.Errorf("mtime = %v, want %v", fi.ModTime(), mtime)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("mode = %v, want existing 0600 preserved", fi.Mode().Perm())
	}
	if names := listDir(t, dir); len(names) != 1 {
		t.Errorf("temp file left behind: %v", names)
	}
}

func TestWriteLocalAtomicFailureKeepsOriginal(t *testing.T) {
	dir := t.TempDir()
	dst := filepath.Join(dir, "index.html")
	if err := os.WriteFile(dst, []byte("good local copy"), 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := writeLocalAtomic(dst, &failingReader{}, nil, time.Time{}); err == nil {
		t.Fatal("expected error from failing reader")
	}

	got, err := os.ReadFile(dst)
	if err != nil {
		t.Fatalf("original file was removed: %v", err)
	}
	if string(got) != "good local copy" {
		t.Errorf("original file was modified: %q", got)
	}
	if names := listDir(t, dir); len(names) != 1 {
		t.Errorf("temp file left behind: %v", names)
	}
}

func TestWriteLocalAtomicFinishErrorAborts(t *testing.T) {
	dir := t.TempDir()
	dst := filepath.Join(dir, "f")

	calls := 0
	finish := func() error { calls++; return errors.New("426 transfer aborted") }
	if _, err := writeLocalAtomic(dst, strings.NewReader("data"), finish, time.Time{}); err == nil {
		t.Fatal("expected finish error to fail the download")
	}
	if calls != 1 {
		t.Errorf("finish called %d times, want 1", calls)
	}
	if _, err := os.Stat(dst); !os.IsNotExist(err) {
		t.Errorf("dst should not exist after aborted transfer, stat err = %v", err)
	}
}

func TestWriteLocalAtomicReplacesSymlinkWithoutFollowing(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "target")
	if err := os.WriteFile(target, []byte("must not change"), 0o644); err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(dir, "link")
	if err := os.Symlink(target, dst); err != nil {
		t.Fatal(err)
	}

	if _, err := writeLocalAtomic(dst, strings.NewReader("downloaded"), nil, time.Time{}); err != nil {
		t.Fatal(err)
	}

	if got, _ := os.ReadFile(target); string(got) != "must not change" {
		t.Errorf("symlink target was written through: %q", got)
	}
	fi, _ := os.Lstat(dst)
	if fi.Mode()&os.ModeSymlink != 0 {
		t.Error("dst is still a symlink")
	}
}

func TestRemotePartialPath(t *testing.T) {
	cases := map[string]string{
		"/www/app.js":     "/www/.app.js.sfync-part",
		"/www/sub/a.html": "/www/sub/.a.html.sfync-part",
		"rel.txt":         ".rel.txt.sfync-part",
	}
	for in, want := range cases {
		if got := remotePartialPath(in); got != want {
			t.Errorf("remotePartialPath(%q) = %q, want %q", in, got, want)
		}
		if !IsPartialName(filepath.Base(remotePartialPath(in))) {
			t.Errorf("partial path for %q not recognized by IsPartialName", in)
		}
	}
	if IsPartialName("app.js") {
		t.Error("ordinary file treated as partial")
	}
}

type fakeFileInfo struct {
	os.FileInfo
	size  int64
	mtime time.Time
}

func (f fakeFileInfo) Size() int64        { return f.size }
func (f fakeFileInfo) ModTime() time.Time { return f.mtime }

func TestCanResumePartial(t *testing.T) {
	now := time.Now()
	local := fakeFileInfo{size: 100, mtime: now}

	tests := []struct {
		name         string
		partialSize  int64
		partialMtime time.Time
		want         bool
	}{
		{"valid prefix written after local mtime", 40, now.Add(time.Minute), true},
		{"complete partial", 100, now.Add(time.Minute), true},
		{"empty partial", 0, now.Add(time.Minute), false},
		{"partial larger than local", 150, now.Add(time.Minute), false},
		{"local modified after partial — stale prefix", 40, now.Add(-time.Minute), false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := canResumePartial(tc.partialSize, tc.partialMtime, local); got != tc.want {
				t.Errorf("got %v, want %v", got, tc.want)
			}
		})
	}
}

func TestWalkLocalSkipsPartialFiles(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "keep.txt"), []byte("x"), 0o644)
	os.WriteFile(filepath.Join(dir, ".keep.txt.123.sfync-part"), []byte("x"), 0o644)

	entries, err := walkLocal(dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := entries["keep.txt"]; !ok {
		t.Error("regular file missing from walk")
	}
	if len(entries) != 1 {
		t.Errorf("partial file included in walk: %v", entries)
	}
}
