package syncignore

import (
	"testing"
)

func TestShouldIgnore(t *testing.T) {
	tests := []struct {
		name     string
		path     string
		patterns []string
		want     bool
	}{
		{
			name:     "exact match",
			path:     ".env",
			patterns: []string{".env"},
			want:     true,
		},
		{
			name:     "glob extension anywhere",
			path:     "src/foo.log",
			patterns: []string{"*.log"},
			want:     true,
		},
		{
			name:     "glob extension at root",
			path:     "debug.log",
			patterns: []string{"*.log"},
			want:     true,
		},
		{
			name:     "doublestar deep path",
			path:     "deep/nested/file.backup",
			patterns: []string{"**/*.backup"},
			want:     true,
		},
		{
			name:     "directory trailing slash matches contents",
			path:     "node_modules/lodash/index.js",
			patterns: []string{"node_modules/"},
			want:     true,
		},
		{
			name:     "directory trailing slash matches the dir itself",
			path:     "node_modules",
			patterns: []string{"node_modules/"},
			want:     true,
		},
		{
			name:     "no match",
			path:     "src/main.go",
			patterns: []string{"*.log", ".env", "node_modules/"},
			want:     false,
		},
		{
			name:     "empty patterns never match",
			path:     "anything.go",
			patterns: []string{},
			want:     false,
		},
		{
			name:     "rooted pattern matches root-level file",
			path:     "dist",
			patterns: []string{"/dist"},
			want:     true,
		},
		{
			name:     "rooted pattern does not match nested occurrence",
			path:     "src/dist",
			patterns: []string{"/dist"},
			want:     false,
		},
		{
			name:     "rooted dir pattern matches contents",
			path:     "dist/bundle.js",
			patterns: []string{"/dist/"},
			want:     true,
		},
		{
			name:     "syncignore itself always excluded",
			path:     ".syncignore",
			patterns: []string{".syncignore"},
			want:     true,
		},
		{
			name:     "git directory",
			path:     ".git/config",
			patterns: []string{".git/"},
			want:     true,
		},
		{
			name:     "nested double-star pattern",
			path:     "a/b/c/d.tmp",
			patterns: []string{"**/*.tmp"},
			want:     true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := ShouldIgnore(tc.path, tc.patterns)
			if got != tc.want {
				t.Errorf("ShouldIgnore(%q, %v) = %v, want %v", tc.path, tc.patterns, got, tc.want)
			}
		})
	}
}

func TestMatchPattern(t *testing.T) {
	tests := []struct {
		name    string
		pattern string
		path    string
		want    bool
	}{
		{"plain name matches at root", "vendor", "vendor", true},
		{"plain name matches nested", "vendor", "a/vendor", true},
		{"plain name matches deep nested", "vendor", "a/b/vendor", true},
		{"plain name no match partial", "vend", "vendor", false},
		{"trailing slash: dir itself", "dist/", "dist", true},
		{"trailing slash: contents", "dist/", "dist/bundle.js", true},
		{"trailing slash: no match sibling", "dist/", "distrib/file.js", false},
		{"rooted: matches at root", "/build", "build", true},
		{"rooted: no match nested", "/build", "src/build", false},
		{"glob extension", "*.go", "main.go", true},
		{"glob extension nested", "*.go", "pkg/main.go", true},
		{"doublestar prefix", "**/test", "a/b/test", true},
		{"doublestar extension", "**/*.log", "logs/app.log", true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := matchPattern(tc.pattern, tc.path)
			if got != tc.want {
				t.Errorf("matchPattern(%q, %q) = %v, want %v", tc.pattern, tc.path, got, tc.want)
			}
		})
	}
}
