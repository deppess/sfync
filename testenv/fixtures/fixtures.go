// Package fixtures generates deterministic local file trees for sfync
// benchmarks and integration tests.
//
// Trees are generated once and cached (keyed by spec and generator
// version) under the system temp dir, since the larger ones take a moment
// to write. Contents are pseudo-random and incompressible — so SSH
// compression or link-level tricks can't flatter the numbers — but fully
// determined by the file path, so runs are reproducible. Every file and
// directory gets a fixed mtime well in the past, so a re-sync of an
// unchanged tree is genuinely a no-op.
package fixtures

import (
	"fmt"
	"hash/fnv"
	"io/fs"
	"math/rand/v2"
	"os"
	"path/filepath"
	"time"
)

// generatorVersion invalidates cached trees when generation logic changes.
const generatorVersion = 1

// CacheRoot is where generated trees are cached. Tests point it at their
// own temp dir so they never write into the shared cache.
var CacheRoot = filepath.Join(os.TempDir(), fmt.Sprintf("sfync-fixtures-v%d", generatorVersion))

// MTime is the timestamp given to every generated file and directory.
var MTime = time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)

// Spec describes a tree shape.
type Spec struct {
	Name    string
	Dirs    int   // directories, spread across Depth levels
	Depth   int   // maximum nesting depth (1 = flat)
	Files   int   // files, distributed round-robin across directories
	MinSize int64 // per-file size range, bytes (inclusive)
	MaxSize int64
}

// Predefined specs, shaped after typical sfync workloads.
var (
	// Website: many small files in a moderately nested tree — the common
	// case, dominated by per-file round trips rather than bandwidth.
	Website = Spec{Name: "website", Dirs: 60, Depth: 4, Files: 1200, MinSize: 512, MaxSize: 24 << 10}
	// Large: a few big files — bandwidth-bound, exercises resume and
	// pipelining.
	Large = Spec{Name: "large", Dirs: 1, Depth: 1, Files: 4, MinSize: 24 << 20, MaxSize: 32 << 20}
	// Mixed: a realistic app deploy — many small files plus some assets.
	Mixed = Spec{Name: "mixed", Dirs: 30, Depth: 3, Files: 400, MinSize: 1 << 10, MaxSize: 192 << 10}
	// Deep: many nested directories with tiny files — stresses mkdir and
	// directory listing rather than data transfer.
	Deep = Spec{Name: "deep", Dirs: 300, Depth: 8, Files: 600, MinSize: 64, MaxSize: 1 << 10}
)

// Specs lists the predefined specs by name.
var Specs = map[string]Spec{
	Website.Name: Website, Large.Name: Large, Mixed.Name: Mixed, Deep.Name: Deep,
}

// Stats summarizes a generated tree.
type Stats struct {
	Files int
	Dirs  int
	Bytes int64
}

// Get returns the path of the cached tree for spec, generating it on first
// use. Callers must treat it as read-only — copy it (Copy) to mutate.
func Get(spec Spec) (string, Stats, error) {
	dir := filepath.Join(CacheRoot, spec.key())
	if st, err := Measure(dir); err == nil && st.Files == spec.Files {
		return dir, st, nil
	}

	// Generate into a scratch dir and rename into place, so a crashed or
	// concurrent generation never leaves a half-written cache entry.
	if err := os.MkdirAll(filepath.Dir(dir), 0o755); err != nil {
		return "", Stats{}, err
	}
	tmp, err := os.MkdirTemp(filepath.Dir(dir), ".gen-*")
	if err != nil {
		return "", Stats{}, err
	}
	defer os.RemoveAll(tmp)
	if err := generate(tmp, spec); err != nil {
		return "", Stats{}, err
	}
	os.RemoveAll(dir)
	if err := os.Rename(tmp, dir); err != nil {
		return "", Stats{}, err
	}
	st, err := Measure(dir)
	return dir, st, err
}

func (s Spec) key() string {
	return fmt.Sprintf("%s-d%d-l%d-f%d-%d-%d", s.Name, s.Dirs, s.Depth, s.Files, s.MinSize, s.MaxSize)
}

func generate(root string, spec Spec) error {
	// Directory layout: dir i lives under dir (i-1)/2-ish, capped at Depth,
	// giving a tree that's both wide and deep.
	dirs := []string{""}
	for i := 1; i < spec.Dirs; i++ {
		parent := dirs[(i-1)/2]
		if depthOf(parent) >= spec.Depth-1 {
			parent = dirs[i%max(1, len(dirs)/4)]
			if depthOf(parent) >= spec.Depth-1 {
				parent = ""
			}
		}
		dirs = append(dirs, filepath.Join(parent, fmt.Sprintf("d%03d", i)))
	}
	for _, d := range dirs[1:] {
		if err := os.MkdirAll(filepath.Join(root, d), 0o755); err != nil {
			return err
		}
	}

	exts := []string{".html", ".css", ".js", ".json", ".svg", ".png", ".txt"}
	for i := range spec.Files {
		rel := filepath.Join(dirs[i%len(dirs)], fmt.Sprintf("f%05d%s", i, exts[i%len(exts)]))
		if err := writeFile(filepath.Join(root, rel), rel, spec); err != nil {
			return err
		}
	}

	// Fixed mtimes on everything (directories last, since writing files
	// updates their parents' mtimes).
	return filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		return os.Chtimes(p, MTime, MTime)
	})
}

func depthOf(rel string) int {
	if rel == "" {
		return 0
	}
	n := 1
	for _, c := range rel {
		if c == filepath.Separator {
			n++
		}
	}
	return n
}

// writeFile writes size-determined, path-seeded pseudo-random content.
func writeFile(path, rel string, spec Spec) error {
	h := fnv.New64a()
	h.Write([]byte(rel))
	seed := h.Sum64()
	rng := rand.New(rand.NewPCG(seed, seed^0x9e3779b97f4a7c15))

	size := spec.MinSize
	if spec.MaxSize > spec.MinSize {
		size += rng.Int64N(spec.MaxSize - spec.MinSize + 1)
	}

	f, err := os.Create(path)
	if err != nil {
		return err
	}
	buf := make([]byte, 64<<10)
	for remaining := size; remaining > 0; {
		n := min(int64(len(buf)), remaining)
		for i := int64(0); i < n; i += 8 {
			v := rng.Uint64()
			for j := int64(0); j < 8 && i+j < n; j++ {
				buf[i+j] = byte(v >> (8 * j))
			}
		}
		if _, err := f.Write(buf[:n]); err != nil {
			f.Close()
			return err
		}
		remaining -= n
	}
	return f.Close()
}

// Measure counts the files, directories and bytes under root (excluding
// root itself).
func Measure(root string) (Stats, error) {
	var st Stats
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if p == root {
			return nil
		}
		if d.IsDir() {
			st.Dirs++
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		st.Files++
		st.Bytes += info.Size()
		return nil
	})
	return st, err
}

// Copy replicates src into dst (which must not exist), preserving modes
// and mtimes — for tests that need a mutable copy of a cached tree.
func Copy(src, dst string) error {
	if err := os.CopyFS(dst, os.DirFS(src)); err != nil {
		return err
	}
	return filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(src, p)
		info, err := d.Info()
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		return os.Chtimes(target, info.ModTime(), info.ModTime())
	})
}
