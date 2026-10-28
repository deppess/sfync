package watcher

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/deppess/sfync/internal/config"
)

// End-to-end through real fsnotify on a temp dir: removing and renaming
// files must schedule their (old) paths, so the queue can decide whether to
// delete them remotely.
func TestWatcherSchedulesRemovedAndRenamedPaths(t *testing.T) {
	ctx, err := filepath.EvalSymlinks(t.TempDir()) // /var → /private/var on macOS
	if err != nil {
		t.Fatal(err)
	}
	removed := filepath.Join(ctx, "removed.html")
	renamed := filepath.Join(ctx, "renamed.html")
	for _, f := range []string{removed, renamed} {
		if err := os.WriteFile(f, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	w, err := New()
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()

	got := make(chan string, 16)
	profile := &config.Profile{Context: ctx, AutoSyncDebounce: 20}
	if err := w.Watch("p", profile, func(p string) { got <- p }); err != nil {
		t.Fatal(err)
	}
	w.Start()

	os.Remove(removed)
	os.Rename(renamed, filepath.Join(ctx, "new-name.html"))

	want := map[string]bool{removed: false, renamed: false}
	deadline := time.After(5 * time.Second)
	for {
		select {
		case p := <-got:
			if _, ok := want[p]; ok {
				want[p] = true
			}
		case <-deadline:
			t.Fatalf("not all paths scheduled: %v", want)
		}
		if want[removed] && want[renamed] {
			return
		}
	}
}

// A context configured with a trailing slash must still match its files.
func TestWatcherContextWithTrailingSlash(t *testing.T) {
	ctx, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	w, err := New()
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()

	got := make(chan string, 4)
	profile := &config.Profile{Context: ctx + "/", AutoSyncDebounce: 20}
	if err := w.Watch("p", profile, func(p string) { got <- p }); err != nil {
		t.Fatal(err)
	}
	w.Start()

	file := filepath.Join(ctx, "index.html")
	os.WriteFile(file, []byte("x"), 0o644)
	select {
	case p := <-got:
		if p != file {
			t.Errorf("scheduled %q, want %q", p, file)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("file in a trailing-slash context was never scheduled")
	}
	if profile.Context != ctx+"/" {
		t.Error("Watch modified the caller's profile")
	}
}

// A directory tree created faster than the watcher can add watches
// (mkdir -p, git checkout, unzip): files anywhere inside must be scheduled,
// and ignored subtrees skipped.
func TestWatcherFindsFilesInNewNestedDirs(t *testing.T) {
	ctx, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(ctx, ".syncignore"), []byte("node_modules/\n"), 0o644)
	w, err := New()
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	got := make(chan string, 16)
	if err := w.Watch("p", &config.Profile{Context: ctx, AutoSyncDebounce: 20}, func(p string) { got <- p }); err != nil {
		t.Fatal(err)
	}
	w.Start()

	deep := filepath.Join(ctx, "a", "b", "c", "d")
	ignored := filepath.Join(ctx, "a", "node_modules", "pkg")
	os.MkdirAll(deep, 0o755)
	os.MkdirAll(ignored, 0o755)
	os.WriteFile(filepath.Join(deep, "deep.js"), []byte("x"), 0o644)
	os.WriteFile(filepath.Join(ignored, "index.js"), []byte("x"), 0o644)

	want := filepath.Join(deep, "deep.js")
	deadline := time.After(5 * time.Second)
	for {
		select {
		case p := <-got:
			if filepath.Dir(p) == ignored {
				t.Errorf("file in ignored subtree scheduled: %s", p)
			}
			if p == want {
				// Give a stray ignored-file event a moment to show up.
				time.Sleep(200 * time.Millisecond)
				select {
				case p := <-got:
					if filepath.Dir(p) == ignored {
						t.Errorf("file in ignored subtree scheduled: %s", p)
					}
				default:
				}
				return
			}
		case <-deadline:
			t.Fatal("file in a newly created nested directory was never scheduled")
		}
	}
}
