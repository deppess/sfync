package transfer

import (
	"io/fs"
	"os"
	"testing"
	"time"
)

// mockFileInfo implements os.FileInfo for testing without touching the filesystem.
type mockFileInfo struct {
	name    string
	size    int64
	modTime time.Time
	isDir   bool
}

func (m mockFileInfo) Name() string       { return m.name }
func (m mockFileInfo) Size() int64        { return m.size }
func (m mockFileInfo) ModTime() time.Time { return m.modTime }
func (m mockFileInfo) IsDir() bool        { return m.isDir }
func (m mockFileInfo) Mode() fs.FileMode  { return 0644 }
func (m mockFileInfo) Sys() any           { return nil }

var _ os.FileInfo = mockFileInfo{}

var epoch = time.Date(2024, 1, 1, 12, 0, 0, 0, time.UTC)

// ── isDifferent ──────────────────────────────────────────────────────────────

func TestIsDifferent(t *testing.T) {
	tests := []struct {
		name   string
		local  os.FileInfo
		remote *RemoteFileInfo
		want   bool
	}{
		{
			name:   "identical size and mtime",
			local:  mockFileInfo{size: 100, modTime: epoch},
			remote: &RemoteFileInfo{Size: 100, ModTime: epoch},
			want:   false,
		},
		{
			name:   "different size",
			local:  mockFileInfo{size: 100, modTime: epoch},
			remote: &RemoteFileInfo{Size: 200, ModTime: epoch},
			want:   true,
		},
		{
			name:   "mtime diff within 2s tolerance",
			local:  mockFileInfo{size: 100, modTime: epoch},
			remote: &RemoteFileInfo{Size: 100, ModTime: epoch.Add(time.Second)},
			want:   false,
		},
		{
			name:   "mtime diff exactly 2s (not different)",
			local:  mockFileInfo{size: 100, modTime: epoch},
			remote: &RemoteFileInfo{Size: 100, ModTime: epoch.Add(2 * time.Second)},
			want:   false,
		},
		{
			name:   "mtime diff over 2s",
			local:  mockFileInfo{size: 100, modTime: epoch},
			remote: &RemoteFileInfo{Size: 100, ModTime: epoch.Add(3 * time.Second)},
			want:   true,
		},
		{
			name:   "negative mtime diff over 2s",
			local:  mockFileInfo{size: 100, modTime: epoch},
			remote: &RemoteFileInfo{Size: 100, ModTime: epoch.Add(-3 * time.Second)},
			want:   true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := isDifferent(tc.local, tc.remote); got != tc.want {
				t.Errorf("isDifferent() = %v, want %v", got, tc.want)
			}
		})
	}
}

// ── diffTrees ────────────────────────────────────────────────────────────────

func actionCounts(actions []MirrorEntry) map[MirrorAction]int {
	m := map[MirrorAction]int{}
	for _, a := range actions {
		m[a.Action]++
	}
	return m
}

func TestDiffTrees_Upload(t *testing.T) {
	local := map[string]localEntry{
		"new.txt":     {info: mockFileInfo{size: 50, modTime: epoch}},
		"changed.txt": {info: mockFileInfo{size: 200, modTime: epoch}},
		"same.txt":    {info: mockFileInfo{size: 100, modTime: epoch}},
		"emptydir":    {info: mockFileInfo{isDir: true}},
	}
	remote := map[string]*RemoteFileInfo{
		"changed.txt": {Size: 100, ModTime: epoch},
		"same.txt":    {Size: 100, ModTime: epoch},
		"deleted.txt": {Size: 10, ModTime: epoch},
		"deldir":      {IsDir: true},
	}

	actions := diffTrees(local, remote, true)
	counts := actionCounts(actions)

	if counts[ActionUpload] != 2 {
		t.Errorf("uploads = %d, want 2", counts[ActionUpload])
	}
	if counts[ActionMkdir] != 1 {
		t.Errorf("mkdirs = %d, want 1", counts[ActionMkdir])
	}
	if counts[ActionSkip] != 1 {
		t.Errorf("skips = %d, want 1", counts[ActionSkip])
	}
	if counts[ActionDelete] != 1 {
		t.Errorf("deletes = %d, want 1", counts[ActionDelete])
	}
	if counts[ActionDeleteDir] != 1 {
		t.Errorf("deletedirs = %d, want 1", counts[ActionDeleteDir])
	}

	// Verify IsNew flag
	for _, a := range actions {
		switch a.RelPath {
		case "new.txt":
			if !a.IsNew {
				t.Error("new.txt: IsNew should be true")
			}
		case "changed.txt":
			if a.IsNew {
				t.Error("changed.txt: IsNew should be false")
			}
		}
	}
}

func TestDiffTrees_Download(t *testing.T) {
	local := map[string]localEntry{
		"existing.txt":  {info: mockFileInfo{size: 100, modTime: epoch}},
		"local-only.txt": {info: mockFileInfo{size: 50, modTime: epoch}},
		"local-dir":     {info: mockFileInfo{isDir: true}},
	}
	remote := map[string]*RemoteFileInfo{
		"existing.txt":   {Size: 100, ModTime: epoch},
		"new-remote.txt": {Size: 200, ModTime: epoch},
		"remote-dir":     {IsDir: true},
	}

	actions := diffTrees(local, remote, false)
	counts := actionCounts(actions)

	if counts[ActionDownload] != 1 {
		t.Errorf("downloads = %d, want 1", counts[ActionDownload])
	}
	if counts[ActionSkip] != 1 {
		t.Errorf("skips = %d, want 1", counts[ActionSkip])
	}
	if counts[ActionMkdir] != 1 {
		t.Errorf("mkdirs = %d, want 1", counts[ActionMkdir])
	}
	if counts[ActionDelete] != 1 {
		t.Errorf("deletes (local-only) = %d, want 1", counts[ActionDelete])
	}
	if counts[ActionDeleteDir] != 1 {
		t.Errorf("deletedirs (local-dir) = %d, want 1", counts[ActionDeleteDir])
	}
}

func TestDiffTrees_Empty(t *testing.T) {
	actions := diffTrees(map[string]localEntry{}, map[string]*RemoteFileInfo{}, true)
	if len(actions) != 0 {
		t.Errorf("empty diff produced %d actions, want 0", len(actions))
	}
}

// ── sortActions ───────────────────────────────────────────────────────────────

func TestSortActions(t *testing.T) {
	actions := []MirrorEntry{
		{Action: ActionDelete, RelPath: "file.txt"},
		{Action: ActionDeleteDir, RelPath: "a/b/c"},
		{Action: ActionDeleteDir, RelPath: "a/b"},
		{Action: ActionDeleteDir, RelPath: "a"},
		{Action: ActionMkdir, RelPath: "x/y"},
		{Action: ActionMkdir, RelPath: "x"},
		{Action: ActionUpload, RelPath: "f.go"},
		{Action: ActionSkip, RelPath: "skip.go"},
	}

	sorted := sortActions(actions)

	// Skips are excluded from output
	for _, a := range sorted {
		if a.Action == ActionSkip {
			t.Error("ActionSkip must not appear in sorted output")
		}
	}

	// Build position index
	pos := map[string]int{}
	for i, a := range sorted {
		pos[a.RelPath] = i
	}

	// Mkdirs: shallowest first
	if pos["x"] >= pos["x/y"] {
		t.Error("mkdir: x should precede x/y (shallowest first)")
	}

	// DeleteDirs: deepest first
	if pos["a/b/c"] >= pos["a/b"] || pos["a/b"] >= pos["a"] {
		t.Error("deletedir: a/b/c should precede a/b should precede a (deepest first)")
	}

	// Phase order: mkdirs < transfers < deletes < deletedirs
	if pos["x"] >= pos["f.go"] {
		t.Error("mkdir should precede upload")
	}
	if pos["f.go"] >= pos["file.txt"] {
		t.Error("upload should precede delete")
	}
	if pos["file.txt"] >= pos["a/b/c"] {
		t.Error("delete should precede deletedir")
	}
}
