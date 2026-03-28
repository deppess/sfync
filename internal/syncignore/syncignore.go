package syncignore

import (
	"bufio"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/bmatcuk/doublestar/v4"
)

// Load reads and parses a .syncignore file from the given context directory.
// Returns a slice of patterns to ignore.
// If the file doesn't exist, returns an empty slice (no ignore rules).
// Malformed patterns are logged as errors and skipped.
func Load(contextPath string) ([]string, error) {
	syncignorePath := filepath.Join(contextPath, ".syncignore")

	// Check if .syncignore exists
	if _, err := os.Stat(syncignorePath); errors.Is(err, fs.ErrNotExist) {
		// No .syncignore file - no ignore rules
		return []string{}, nil
	}

	file, err := os.Open(syncignorePath)
	if err != nil {
		return nil, fmt.Errorf("failed to open .syncignore: %w", err)
	}
	defer file.Close()

	var patterns []string
	scanner := bufio.NewScanner(file)
	lineNum := 0

	for scanner.Scan() {
		lineNum++
		line := strings.TrimSpace(scanner.Text())

		// Skip empty lines
		if line == "" {
			continue
		}

		// Skip comments
		if strings.HasPrefix(line, "#") {
			continue
		}

		// Validate pattern (basic check - doublestar will validate during matching)
		// Just ensure it's not obviously malformed
		if strings.Contains(line, "***") {
			fmt.Fprintf(os.Stderr, "Warning: Invalid pattern '%s' in .syncignore line %d (skipping)\n", line, lineNum)
			continue
		}

		patterns = append(patterns, line)
	}

	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("failed to read .syncignore: %w", err)
	}

	// Always add .syncignore itself
	patterns = append(patterns, ".syncignore")

	return patterns, nil
}

// ShouldIgnore checks if a relative path matches any ignore pattern.
// relativePath should be relative to the context directory.
func ShouldIgnore(relativePath string, patterns []string) bool {
	relativePath = filepath.ToSlash(relativePath)
	for _, pattern := range patterns {
		if matchPattern(filepath.ToSlash(pattern), relativePath) {
			return true
		}
	}
	return false
}

// matchPattern tests a single gitignore-style pattern against a relative path.
// Handles: trailing-slash dir patterns, leading-slash root-anchored patterns,
// **/ global patterns, and plain patterns (matched at root and anywhere).
func matchPattern(pattern, relPath string) bool {
	isDir := strings.HasSuffix(pattern, "/")
	if isDir {
		pattern = pattern[:len(pattern)-1]
	}
	isRooted := strings.HasPrefix(pattern, "/")
	if isRooted {
		pattern = pattern[1:]
	}

	try := func(p string) bool {
		m, _ := doublestar.Match(p, relPath)
		return m
	}

	// Root-anchored or already global (e.g. **/foo): test as-is only
	if isRooted || strings.HasPrefix(pattern, "**/") {
		return try(pattern) || (isDir && try(pattern+"/**"))
	}
	// Plain pattern: match at root level AND anywhere in the tree
	return try(pattern) || try("**/"+pattern) ||
		(isDir && (try(pattern+"/**") || try("**/"+pattern+"/**")))
}
