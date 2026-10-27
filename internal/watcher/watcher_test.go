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
