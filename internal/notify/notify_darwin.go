package notify

import (
	"embed"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// The compiled, ad-hoc-signed sfync-notifier.app is committed to the repo
// (see darwinapp/build.sh) and embedded directly into the sfync binary, so
// a plain `go build` never needs the Swift toolchain — only rebuilding the
// helper itself (main.swift/Info.plist/icon changes) does.
//
//go:embed darwinapp/sfync-notifier.app
var notifierAppFS embed.FS

const notifierAppEmbedRoot = "darwinapp/sfync-notifier.app"

// Send posts a notification via the bundled sfync-notifier.app helper. A
// real signed app bundle is required for UNUserNotificationCenter to have
// a stable identity macOS can grant (and remember) permission against — a
// bare binary, or a raw `osascript -e display notification` call, doesn't
// have one, which is why that approach is unreliable, especially from a
// headless launchd context.
func Send(title, message string, urgency Urgency) {
	if disabled() {
		return
	}
	if err := EnsureReady(); err != nil {
		return
	}
	binPath, err := notifierBinaryPath()
	if err != nil {
		return
	}
	exec.Command(binPath, title, message, string(urgency)).Run()
}

// EnsureReady extracts the bundled notifier app into ~/Library/Application
// Support/sfync if it isn't already present. Cheap no-op on the common
// path (just a stat), so it's safe to call before every notification.
// With notifications disabled there is nothing to prepare.
func EnsureReady() error {
	if disabled() {
		return nil
	}
	binPath, err := notifierBinaryPath()
	if err != nil {
		return err
	}
	if _, err := os.Stat(binPath); err == nil {
		return nil
	}
	return extractNotifierApp()
}

// Reinstall force re-extracts the bundled notifier app, overwriting
// whatever's on disk. Called from `install-daemon` so upgrades stay in
// sync with whatever version of sfync built them.
func Reinstall() error {
	return extractNotifierApp()
}

// TestNotification synchronously sends a real notification and reports
// whether it succeeded — used by `install-daemon` to surface the one-time
// system permission prompt (and any failure) directly to the user instead
// of failing silently later from inside the daemon.
func TestNotification() error {
	if err := EnsureReady(); err != nil {
		return err
	}
	binPath, err := notifierBinaryPath()
	if err != nil {
		return err
	}
	out, err := exec.Command(binPath, "sfync", "Notifications are enabled", "normal").CombinedOutput()
	if err != nil {
		if msg := strings.TrimSpace(string(out)); msg != "" {
			return fmt.Errorf("%s", msg)
		}
		return err
	}
	return nil
}

// notifierSupportDir uses a `.noindex`-suffixed directory name — the
// reliable way to keep a bundle out of Spotlight. `.metadata_never_index`
// (still dropped below as a belt-and-suspenders measure) turned out not to
// be honored here: Spotlight's bundle-detection importer registers any
// discovered .app with LaunchServices independently of normal content
// indexing, so plain content-index exclusion isn't enough to keep an .app
// out of search results — the containing directory itself has to be
// excluded from indexing altogether.
func notifierSupportDir() (string, error) {
	homeDir, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("cannot determine home directory: %w", err)
	}
	return filepath.Join(homeDir, "Library", "Application Support", "sfync.noindex"), nil
}

// removeLegacySupportDir cleans up the pre-.noindex install location from
// earlier sfync versions, best-effort — its .app bundle would otherwise sit
// around still registered with Spotlight/LaunchServices.
func removeLegacySupportDir() {
	homeDir, err := os.UserHomeDir()
	if err != nil {
		return
	}
	os.RemoveAll(filepath.Join(homeDir, "Library", "Application Support", "sfync"))
}

func notifierAppPath() (string, error) {
	dir, err := notifierSupportDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "sfync-notifier.app"), nil
}

func notifierBinaryPath() (string, error) {
	appPath, err := notifierAppPath()
	if err != nil {
		return "", err
	}
	return filepath.Join(appPath, "Contents", "MacOS", "sfync-notifier"), nil
}

// extractNotifierApp copies the embedded app bundle out to disk and swaps
// it into place atomically. It builds the bundle in a scratch directory
// first, then uses os.Rename to install it — never a RemoveAll+rewrite
// directly on the live path. That matters because extraction can race a
// concurrent process: `install-daemon` re-extracting while the daemon's own
// Send() call (a separate `sfync daemon` process) is doing its own
// EnsureReady() check at the same instant. A direct remove-then-rewrite
// would risk that second process seeing a missing or half-written bundle
// mid-swap — the same class of "replaced a binary out from under a running
// process" problem as overwriting an in-use executable with cp instead of
// mv. Two renames (old bundle aside, new bundle in) keep each step atomic;
// embed.FS strips executable bits, so the inner Mach-O is re-chmod'd
// explicitly while building the scratch copy.
func extractNotifierApp() error {
	supportDir, err := notifierSupportDir()
	if err != nil {
		return err
	}
	appPath, err := notifierAppPath()
	if err != nil {
		return err
	}

	if err := os.MkdirAll(supportDir, 0o755); err != nil {
		return fmt.Errorf("cannot create %s: %w", supportDir, err)
	}
	if err := ensureSpotlightExcluded(supportDir); err != nil {
		return err
	}
	removeLegacySupportDir()

	scratchDir, err := os.MkdirTemp(supportDir, ".extract-*")
	if err != nil {
		return fmt.Errorf("cannot create scratch directory: %w", err)
	}
	defer os.RemoveAll(scratchDir)

	scratchAppPath := filepath.Join(scratchDir, "sfync-notifier.app")
	err = fs.WalkDir(notifierAppFS, notifierAppEmbedRoot, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(notifierAppEmbedRoot, path)
		if err != nil {
			return err
		}
		dest := filepath.Join(scratchAppPath, rel)
		if d.IsDir() {
			return os.MkdirAll(dest, 0o755)
		}
		data, err := notifierAppFS.ReadFile(path)
		if err != nil {
			return err
		}
		mode := os.FileMode(0o644)
		if d.Name() == "sfync-notifier" {
			mode = 0o755
		}
		return os.WriteFile(dest, data, mode)
	})
	if err != nil {
		return fmt.Errorf("failed to extract notifier app: %w", err)
	}

	// Swap into place: move the old bundle aside (if present) before moving
	// the new one in — os.Rename can't overwrite a non-empty directory
	// directly, but each individual rename is atomic.
	stalePath := appPath + ".stale"
	os.RemoveAll(stalePath)
	if _, statErr := os.Lstat(appPath); statErr == nil {
		if err := os.Rename(appPath, stalePath); err != nil {
			return fmt.Errorf("failed to move aside existing notifier app: %w", err)
		}
	}
	if err := os.Rename(scratchAppPath, appPath); err != nil {
		return fmt.Errorf("failed to install notifier app: %w", err)
	}
	os.RemoveAll(stalePath)

	return nil
}

// ensureSpotlightExcluded drops a `.metadata_never_index` sentinel — the
// standard macOS convention for telling Spotlight to skip everything under
// a directory — so this background helper app never shows up in Spotlight
// or Siri search results.
func ensureSpotlightExcluded(dir string) error {
	sentinel := filepath.Join(dir, ".metadata_never_index")
	if _, err := os.Stat(sentinel); err == nil {
		return nil
	}
	return os.WriteFile(sentinel, nil, 0o644)
}
