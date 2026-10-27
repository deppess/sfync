package watcher

import (
	"os"
	"path/filepath"
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
