package watcher

import "testing"

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
