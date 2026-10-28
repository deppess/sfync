package cmd

import (
	"os"
	"path/filepath"
	"testing"
)

func TestEscapesBase(t *testing.T) {
	cases := map[string]bool{
		"..":            true,
		"../x":          true,
		"../../etc":     true,
		"..env.local":   false, // a name starting with two dots is inside
		"..config/a.js": false,
		"a/../b":        false,
		".":             false,
		"sub/file":      false,
	}
	for rel, want := range cases {
		if got := escapesBase(rel); got != want {
			t.Errorf("escapesBase(%q) = %v, want %v", rel, got, want)
		}
	}
}

func TestContextRel(t *testing.T) {
	real := t.TempDir()
	if err := os.MkdirAll(filepath.Join(real, "site", "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	// link is a second spelling of real, as /var is of /private/var.
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	outside := t.TempDir()
	// A symlink inside the context pointing out keeps its own name.
	if err := os.Symlink(outside, filepath.Join(real, "site", "out")); err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		context, path, want string
		ok                  bool
	}{
		{filepath.Join(real, "site"), filepath.Join(real, "site", "a.txt"), "a.txt", true},
		{filepath.Join(link, "site"), filepath.Join(real, "site", "sub", "a.txt"), filepath.Join("sub", "a.txt"), true},
		{filepath.Join(real, "site"), filepath.Join(link, "site", "sub", "a.txt"), filepath.Join("sub", "a.txt"), true},
		// Not created yet (a pull target), under a symlinked spelling.
		{filepath.Join(real, "site"), filepath.Join(link, "site", "new", "dir", "b.txt"), filepath.Join("new", "dir", "b.txt"), true},
		{filepath.Join(real, "site") + "/", filepath.Join(real, "site", "a.txt"), "a.txt", true},
		{filepath.Join(real, "site"), filepath.Join(real, "site", "out", "x"), filepath.Join("out", "x"), true},
		{filepath.Join(real, "site"), filepath.Join(real, "site"), "", false},
		{filepath.Join(link, "site"), filepath.Join(real, "site"), "", false},
		{filepath.Join(real, "site"), filepath.Join(real, "other.txt"), "", false},
		{filepath.Join(link, "site"), filepath.Join(outside, "a.txt"), "", false},
	}
	for _, c := range cases {
		got, ok := contextRel(c.context, c.path)
		if got != c.want || ok != c.ok {
			t.Errorf("contextRel(%q, %q) = %q, %v; want %q, %v", c.context, c.path, got, ok, c.want, c.ok)
		}
	}
}
