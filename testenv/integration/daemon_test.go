package integration

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/deppess/sfync/internal/config"
	"github.com/deppess/sfync/internal/transfer"
	"github.com/deppess/sfync/internal/watcher"
	"github.com/deppess/sfync/testenv/netem"
)

// startDaemon wires the real daemon pipeline — fsnotify watcher, debouncer,
// upload queue, connection pool — for one profile, as `sfync daemon` does.
func startDaemon(t *testing.T, profile *config.Profile) {
	t.Helper()
	profiles := map[string]*config.Profile{"p": profile}
	pool := transfer.NewConnectionPool(profiles)
	q := watcher.NewUploadQueue(profiles, pool)
	q.Start(func(_, _ string) {}, func(_, rel string, err error, _ int) {
		t.Errorf("daemon failed on %s: %v", rel, err)
	})
	w, err := watcher.New()
	if err != nil {
		t.Fatal(err)
	}
	if err := w.Watch("p", profile, func(p string) { q.Enqueue("p", p) }); err != nil {
		t.Fatal(err)
	}
	w.Start()
	t.Cleanup(func() {
		w.Close()
		q.Stop()
		pool.CloseAll()
	})
}

// eventually polls cond until it holds or the deadline passes.
func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for: %s", what)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func remoteHas(root, rel, content string) func() bool {
	return func() bool {
		got, err := os.ReadFile(filepath.Join(root, rel))
		return err == nil && string(got) == content
	}
}

func remoteGone(root, rel string) func() bool {
	return func() bool {
		_, err := os.Stat(filepath.Join(root, rel))
		return os.IsNotExist(err)
	}
}

func TestDaemonEndToEnd(t *testing.T) {
	for _, k := range kinds {
		t.Run(k.name, func(t *testing.T) {
			srv := k.start(t, netem.Local)
			local, err := filepath.EvalSymlinks(t.TempDir()) // fsnotify reports real paths
			if err != nil {
				t.Fatal(err)
			}
			profile := srv.Profile(local)
			profile.AutoSync = true
			profile.AutoSyncDeletes = true
			profile.AutoSyncDebounce = 50
			startDaemon(t, profile)

			// New file, and a file inside a new nested directory.
			os.WriteFile(filepath.Join(local, "index.html"), []byte("v1"), 0o644)
			eventually(t, "index.html uploaded", remoteHas(srv.Root, "index.html", "v1"))
			// Created in one go, faster than the watcher can add the new
			// directories (as mkdir -p, git checkout or unzip do): the files
			// inside must still be found.
			os.MkdirAll(filepath.Join(local, "assets", "css"), 0o755)
			os.WriteFile(filepath.Join(local, "assets", "css", "site.css"), []byte("body{}"), 0o644)
			eventually(t, "nested file uploaded", remoteHas(srv.Root, "assets/css/site.css", "body{}"))

			// Edit.
			os.WriteFile(filepath.Join(local, "index.html"), []byte("v2"), 0o644)
			eventually(t, "edit uploaded", remoteHas(srv.Root, "index.html", "v2"))

			// Editor-style atomic save: write a temp file, rename it over the
			// original. The final content must arrive; the temp must not.
			tmp := filepath.Join(local, ".index.html.swp")
			os.WriteFile(tmp, []byte("v3"), 0o644)
			os.Rename(tmp, filepath.Join(local, "index.html"))
			eventually(t, "atomic save uploaded", remoteHas(srv.Root, "index.html", "v3"))

			// Delete and rename propagate (autoSyncDeletes).
			os.Remove(filepath.Join(local, "assets", "css", "site.css"))
			eventually(t, "delete propagated", remoteGone(srv.Root, "assets/css/site.css"))
			os.Rename(filepath.Join(local, "index.html"), filepath.Join(local, "home.html"))
			eventually(t, "rename propagated", func() bool {
				return remoteHas(srv.Root, "home.html", "v3")() && remoteGone(srv.Root, "index.html")()
			})

			// Nothing transient left behind remotely.
			time.Sleep(300 * time.Millisecond)
			if _, err := os.Stat(filepath.Join(srv.Root, ".index.html.swp")); err == nil {
				t.Error("editor temp file was uploaded and left on the server")
			}
			noPartials(t, srv.Root)
		})
	}
}
