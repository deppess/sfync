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
		// FTP LIST: remote time truncated to the minute.
		{
			name:   "minute precision: local seconds within the listed minute",
			local:  mockFileInfo{size: 100, modTime: epoch.Add(37 * time.Second)},
			remote: &RemoteFileInfo{Size: 100, ModTime: epoch, ModTimePrecision: time.Minute},
			want:   false,
		},
		{
			name:   "minute precision: local in the next minute",
			local:  mockFileInfo{size: 100, modTime: epoch.Add(65 * time.Second)},
			remote: &RemoteFileInfo{Size: 100, ModTime: epoch, ModTimePrecision: time.Minute},
			want:   true,
		},
		{
			name:   "minute precision: local before the listed minute",
			local:  mockFileInfo{size: 100, modTime: epoch.Add(-10 * time.Second)},
			remote: &RemoteFileInfo{Size: 100, ModTime: epoch, ModTimePrecision: time.Minute},
			want:   true,
		},
		{
			name:   "minute precision: size still compared exactly",
			local:  mockFileInfo{size: 101, modTime: epoch.Add(10 * time.Second)},
			remote: &RemoteFileInfo{Size: 100, ModTime: epoch, ModTimePrecision: time.Minute},
			want:   true,
		},
		// FTP LIST for old files: date only, parsed as midnight.
		{
			name:   "day precision: same day",
			local:  mockFileInfo{size: 100, modTime: epoch.Add(9 * time.Hour)},
			remote: &RemoteFileInfo{Size: 100, ModTime: epoch.Truncate(24 * time.Hour), ModTimePrecision: 24 * time.Hour},
			want:   false,
		},
		{
			name:   "day precision: next day",
			local:  mockFileInfo{size: 100, modTime: epoch.Truncate(24 * time.Hour).Add(25 * time.Hour)},
			remote: &RemoteFileInfo{Size: 100, ModTime: epoch.Truncate(24 * time.Hour), ModTimePrecision: 24 * time.Hour},
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
		"existing.txt":   {info: mockFileInfo{size: 100, modTime: epoch}},
		"local-only.txt": {info: mockFileInfo{size: 50, modTime: epoch}},
		"local-dir":      {info: mockFileInfo{isDir: true}},
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

func TestDiffTrees_TypeConflict(t *testing.T) {
	t.Run("upload: local file, remote dir", func(t *testing.T) {
		local := map[string]localEntry{
			"foo": {info: mockFileInfo{name: "foo", size: 10, modTime: epoch}},
		}
		remote := map[string]*RemoteFileInfo{
			"foo": {IsDir: true},
		}
		actions := diffTrees(local, remote, true)
		counts := actionCounts(actions)
		if counts[ActionConflict] != 1 {
			t.Errorf("conflicts = %d, want 1", counts[ActionConflict])
		}
		if counts[ActionUpload] != 0 {
			t.Errorf("uploads = %d, want 0 (should not attempt to overwrite a directory)", counts[ActionUpload])
		}
	})

	t.Run("upload: local dir, remote file", func(t *testing.T) {
		local := map[string]localEntry{
			"foo": {info: mockFileInfo{name: "foo", isDir: true}},
		}
		remote := map[string]*RemoteFileInfo{
			"foo": {IsDir: false},
		}
		actions := diffTrees(local, remote, true)
		counts := actionCounts(actions)
		if counts[ActionConflict] != 1 {
			t.Errorf("conflicts = %d, want 1", counts[ActionConflict])
		}
		if counts[ActionMkdir] != 0 {
			t.Errorf("mkdirs = %d, want 0 (should not attempt to mkdir over a file)", counts[ActionMkdir])
		}
	})

	t.Run("download: remote file, local dir", func(t *testing.T) {
		local := map[string]localEntry{
			"foo": {info: mockFileInfo{name: "foo", isDir: true}},
		}
		remote := map[string]*RemoteFileInfo{
			"foo": {IsDir: false},
		}
		actions := diffTrees(local, remote, false)
		counts := actionCounts(actions)
		if counts[ActionConflict] != 1 {
			t.Errorf("conflicts = %d, want 1", counts[ActionConflict])
		}
		if counts[ActionDownload] != 0 {
			t.Errorf("downloads = %d, want 0 (should not attempt to overwrite a directory)", counts[ActionDownload])
		}
	})

	t.Run("download: remote dir, local file", func(t *testing.T) {
		local := map[string]localEntry{
			"foo": {info: mockFileInfo{name: "foo", size: 10, modTime: epoch}},
		}
		remote := map[string]*RemoteFileInfo{
			"foo": {IsDir: true},
		}
		actions := diffTrees(local, remote, false)
		counts := actionCounts(actions)
		if counts[ActionConflict] != 1 {
			t.Errorf("conflicts = %d, want 1", counts[ActionConflict])
		}
		if counts[ActionMkdir] != 0 {
			t.Errorf("mkdirs = %d, want 0 (should not attempt to mkdir over a file)", counts[ActionMkdir])
		}
	})
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

func TestListTimePrecision(t *testing.T) {
	now := time.Date(2026, 6, 15, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		name    string
		t       time.Time
		precise bool
		want    time.Duration
	}{
		{"MLSD is exact", now.AddDate(-2, 0, 0), true, 0},
		{"recent LIST entry", now.Add(-48 * time.Hour), false, time.Minute},
		{"old LIST entry (date only)", now.AddDate(0, -7, 0), false, 24 * time.Hour},
		{"5-6 months old treated as date-only", now.AddDate(0, -5, -15), false, 24 * time.Hour},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := listTimePrecision(tc.t, tc.precise, now); got != tc.want {
				t.Errorf("got %v, want %v", got, tc.want)
			}
		})
	}
}

func TestListOffsetFrom(t *testing.T) {
	utc := time.Date(2026, 10, 27, 20, 14, 37, 0, time.UTC)
	tests := []struct {
		name   string
		offset time.Duration
	}{
		{"UTC", 0},
		{"PDT (UTC-7)", -7 * time.Hour},
		{"CET (UTC+1)", time.Hour},
		{"India (UTC+5:30)", 5*time.Hour + 30*time.Minute},
		{"Nepal (UTC+5:45)", 5*time.Hour + 45*time.Minute},
		{"Kiribati (UTC+14)", 14 * time.Hour},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			// What a LIST shows: server wall clock, truncated to the minute,
			// parsed as UTC by the client.
			listed := utc.Add(tc.offset).Truncate(time.Minute)
			got, ok := listOffsetFrom(listed, utc)
			if !ok || got != tc.offset {
				t.Errorf("got %v ok=%v, want %v", got, ok, tc.offset)
			}
		})
	}
	if _, ok := listOffsetFrom(utc.Add(20*time.Hour), utc); ok {
		t.Error("20h offset accepted as plausible")
	}
}
