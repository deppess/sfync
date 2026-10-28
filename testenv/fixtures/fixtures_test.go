package fixtures

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

func TestGetIsDeterministicAndCached(t *testing.T) {
	orig := CacheRoot
	CacheRoot = t.TempDir()
	t.Cleanup(func() { CacheRoot = orig })

	spec := Spec{Name: "unit", Dirs: 12, Depth: 4, Files: 40, MinSize: 10, MaxSize: 3000}
	dir, st, err := Get(spec)
	if err != nil {
		t.Fatal(err)
	}
	if st.Files != 40 || st.Dirs != 11 {
		t.Errorf("stats = %+v, want 40 files, 11 dirs", st)
	}

	// Regenerating elsewhere yields byte-identical content.
	other := t.TempDir()
	if err := generate(other, spec); err != nil {
		t.Fatal(err)
	}
	filepathWalkFiles(t, dir, func(rel string, data []byte) {
		got, err := os.ReadFile(filepath.Join(other, rel))
		if err != nil || !bytes.Equal(got, data) {
			t.Errorf("%s differs between generations", rel)
		}
	})

	// Fixed mtimes everywhere, so re-syncs are no-ops.
	filepath.Walk(dir, func(p string, info os.FileInfo, err error) error {
		if p != dir && !info.ModTime().Equal(MTime) {
			t.Errorf("%s mtime = %v", p, info.ModTime())
		}
		return nil
	})

	// Second Get hits the cache (same path, no regeneration).
	dir2, _, err := Get(spec)
	if err != nil || dir2 != dir {
		t.Errorf("cache miss: %q vs %q (%v)", dir2, dir, err)
	}
}

func TestPredefinedSpecsRespectDepth(t *testing.T) {
	for _, spec := range []Spec{Website, Mixed, Deep} {
		s := spec
		s.Files, s.MinSize, s.MaxSize = 0, 0, 0 // shape only, no data
		root := t.TempDir()
		if err := generate(root, s); err != nil {
			t.Fatal(err)
		}
		maxDepth := 0
		filepath.Walk(root, func(p string, info os.FileInfo, err error) error {
			rel, _ := filepath.Rel(root, p)
			if rel != "." {
				maxDepth = max(maxDepth, depthOf(rel))
			}
			return nil
		})
		if maxDepth > spec.Depth || maxDepth < min(spec.Depth, 2) {
			t.Errorf("%s: max depth %d, want <= %d", spec.Name, maxDepth, spec.Depth)
		}
	}
}

func filepathWalkFiles(t *testing.T, root string, fn func(rel string, data []byte)) {
	t.Helper()
	filepath.Walk(root, func(p string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return err
		}
		rel, _ := filepath.Rel(root, p)
		data, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		fn(rel, data)
		return nil
	})
}
