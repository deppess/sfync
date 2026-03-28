package cmd

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/deppess/sfync/internal/config"
	"github.com/deppess/sfync/internal/deps"
	"github.com/deppess/sfync/internal/mount"
	"github.com/deppess/sfync/internal/notify"
)

// detectTerminal returns the terminal emulator binary to use, checking
// $TERMINAL first, then terminal-specific env vars, then $TERM.
func detectTerminal() string {
	if t := os.Getenv("TERMINAL"); t != "" {
		return t
	}

	// Terminal-specific environment variables
	if os.Getenv("KITTY_WINDOW_ID") != "" {
		return "kitty"
	}
	if os.Getenv("GHOSTTY_RESOURCES_DIR") != "" || os.Getenv("GHOSTTY_BIN_DIR") != "" {
		return "ghostty"
	}
	if os.Getenv("WEZTERM_PANE") != "" || os.Getenv("WEZTERM_UNIX_SOCKET") != "" {
		return "wezterm"
	}
	if os.Getenv("ALACRITTY_WINDOW_ID") != "" || os.Getenv("ALACRITTY_SOCKET") != "" {
		return "alacritty"
	}
	if os.Getenv("FOOT_SERVER_SOCKET") != "" {
		return "foot"
	}

	// Fall back to $TERM
	switch os.Getenv("TERM") {
	case "xterm-kitty":
		return "kitty"
	case "foot":
		return "foot"
	case "alacritty":
		return "alacritty"
	case "xterm-ghostty":
		return "ghostty"
	case "wezterm":
		return "wezterm"
	}

	return "kitty"
}

// buildTerminalCmd constructs the exec.Cmd for launching a terminal with a program.
// Each terminal has different flags for setting the window title and running a command.
func buildTerminalCmd(terminal, title, program string, args ...string) *exec.Cmd {
	base := filepath.Base(terminal)
	switch base {
	case "kitty":
		// kitty -T <title> -e <program> [args...]
		cmdArgs := append([]string{"-T", title, "-e", program}, args...)
		return exec.Command(terminal, cmdArgs...)
	case "ghostty":
		// ghostty --title=<title> -e <program> [args...]
		cmdArgs := append([]string{"--title=" + title, "-e", program}, args...)
		return exec.Command(terminal, cmdArgs...)
	case "alacritty":
		// alacritty --title <title> -e <program> [args...]
		cmdArgs := append([]string{"--title", title, "-e", program}, args...)
		return exec.Command(terminal, cmdArgs...)
	case "foot", "footclient":
		// foot --title=<title> <program> [args...]  (no -e flag; command appended directly)
		cmdArgs := append([]string{"--title=" + title, program}, args...)
		return exec.Command(terminal, cmdArgs...)
	case "wezterm":
		// wezterm start -- <program> [args...]  (no --title flag available at launch)
		cmdArgs := append([]string{"start", "--", program}, args...)
		return exec.Command(terminal, cmdArgs...)
	default:
		// Generic fallback: try -e
		cmdArgs := append([]string{"-e", program}, args...)
		return exec.Command(terminal, cmdArgs...)
	}
}

// Mount mounts a remote filesystem
func Mount(profileName string, openYazi bool) error {
	// Load config
	cfg, err := config.Load()
	if err != nil {
		notify.Error("SFTP Sync Error", err.Error())
		return err
	}

	// Get profile
	profile, err := cfg.GetProfile(profileName)
	if err != nil {
		notify.Error("SFTP Sync Error", err.Error())
		return err
	}

	// Check protocol-specific dependencies
	if profile.Protocol == "sftp" {
		if err := deps.CheckRequired("sshfs", "notify-send"); err != nil {
			notify.Error("Mount Error", err.Error())
			return err
		}
	} else {
		if err := deps.CheckRequired("rclone", "notify-send"); err != nil {
			notify.Error("Mount Error", err.Error())
			return err
		}
	}

	// Check yazi and the detected terminal if needed
	if openYazi {
		if err := deps.CheckRequired("yazi"); err != nil {
			notify.Error("Mount Error", err.Error())
			return err
		}
	}

	// Perform mount
	notify.Info("SFTP Mount", fmt.Sprintf("Mounting %s...", profileName))

	if err := mount.Mount(profileName, profile); err != nil {
		// Detailed error notification
		errorMsg := err.Error()
		if strings.Contains(errorMsg, "already mounted") {
			mountPoint, err := mount.GetMountPoint(profileName, profile)
			if err != nil {
				mountPoint = "(unknown location)"
			}
			notify.Error("Mount Error", fmt.Sprintf("Profile '%s' is already mounted at %s", profileName, mountPoint))
		} else if strings.Contains(errorMsg, "unreachable") {
			notify.Error("Mount Error", fmt.Sprintf("Cannot reach %s:%d\n%s", profile.Host, profile.Port, errorMsg))
		} else if strings.Contains(errorMsg, "authentication") {
			notify.Error("Mount Error", fmt.Sprintf("Authentication failed for %s@%s", profile.Username, profile.Host))
		} else {
			notify.Error("Mount Error", fmt.Sprintf("Failed to mount %s\n%s", profileName, errorMsg))
		}
		return err
	}

	mountPoint, err := mount.GetMountPoint(profileName, profile)
	if err != nil {
		notify.Error("Mount Error", err.Error())
		return err
	}

	if openYazi {
		terminal := detectTerminal()

		// Verify the terminal binary is available
		if _, err := exec.LookPath(terminal); err != nil {
			errMsg := fmt.Sprintf("Terminal '%s' not found in PATH", terminal)
			notify.Error("Mount Error", errMsg)
			return fmt.Errorf("%s", errMsg)
		}

		notify.Success("SFTP Mount", fmt.Sprintf("Mounted %s at %s\nOpening yazi...", profileName, mountPoint))
		fmt.Printf("✓ Mounted %s at %s\n", profileName, mountPoint)
		fmt.Printf("Opening yazi in %s...\n", filepath.Base(terminal))

		title := fmt.Sprintf("SFTP-Mount-%s", profileName)
		cmd := buildTerminalCmd(terminal, title, "yazi", mountPoint)

		// Run and wait for the terminal to exit
		if err := cmd.Run(); err != nil {
			fmt.Fprintf(os.Stderr, "Warning: yazi exited with error: %v\n", err)
		}

		// Auto-unmount when yazi exits
		fmt.Println("Yazi closed, unmounting...")
		if err := mount.Unmount(profileName, profile); err != nil {
			notify.Error("Unmount Error", err.Error())
			return err
		}

		notify.Success("SFTP Unmount", fmt.Sprintf("Unmounted %s", profileName))
		fmt.Printf("✓ Unmounted %s\n", profileName)
	} else {
		// Just mount and print path
		notify.Success("SFTP Mount", fmt.Sprintf("Mounted %s at %s", profileName, mountPoint))
		fmt.Printf("✓ Mounted %s at:\n%s\n", profileName, mountPoint)
	}

	return nil
}

// Unmount unmounts a profile's filesystem
func Unmount(profileName string, unmountAll bool) error {
	if unmountAll {
		// Unmount all profiles
		mounted, err := mount.ListMounted()
		if err != nil {
			notify.Error("Unmount Error", err.Error())
			return err
		}

		if len(mounted) == 0 {
			fmt.Println("No mounted profiles")
			return nil
		}

		fmt.Printf("Unmounting %d profile(s)...\n", len(mounted))
		if err := mount.UnmountAll(); err != nil {
			notify.Error("Unmount Error", err.Error())
			return err
		}

		notify.Success("SFTP Unmount", fmt.Sprintf("Unmounted %d profile(s)", len(mounted)))
		fmt.Printf("✓ Unmounted %d profile(s)\n", len(mounted))
		return nil
	}

	// Load config to get profile (needed for custom context paths)
	cfg, err := config.Load()
	if err != nil {
		notify.Error("SFTP Sync Error", err.Error())
		return err
	}

	profile, err := cfg.GetProfile(profileName)
	if err != nil {
		notify.Error("SFTP Sync Error", err.Error())
		return err
	}

	// Unmount single profile
	if err := mount.Unmount(profileName, profile); err != nil {
		notify.Error("Unmount Error", err.Error())
		return err
	}

	notify.Success("SFTP Unmount", fmt.Sprintf("Unmounted %s", profileName))
	fmt.Printf("✓ Unmounted %s\n", profileName)
	return nil
}

// Mounts lists all currently mounted profiles
func Mounts() error {
	// Collect mounted profiles from two sources:
	// 1. Default ~/.mounted/<name> directories (managed by sftp-sync)
	// 2. Context-based mount points from config (user-specified directories)
	managed, err := mount.ListMounted()
	if err != nil {
		return err
	}

	// Build a set of already-found names to avoid duplicates
	found := make(map[string]string) // profileName → mountPoint
	for _, profileName := range managed {
		mountPoint, mpErr := mount.GetMountPoint(profileName, nil)
		if mpErr != nil {
			mountPoint = "(unknown)"
		}
		found[profileName] = mountPoint
	}

	// Also check profiles with a context set — they mount at the context path
	// and ListMounted() cannot see them since it only scans ~/.mounted/.
	cfg, cfgErr := config.Load()
	if cfgErr == nil {
		for name, profile := range cfg.Profiles {
			p := profile // avoid loop-variable capture
			if p.Context == "" {
				continue
			}
			if _, alreadySeen := found[name]; alreadySeen {
				continue
			}
			if mount.IsMounted(name, &p) {
				found[name] = p.Context
			}
		}
	}

	if len(found) == 0 {
		fmt.Println("No mounted profiles")
		return nil
	}

	fmt.Printf("Currently mounted profiles (%d):\n", len(found))
	for profileName, mountPoint := range found {
		fmt.Printf("  • %s → %s\n", profileName, mountPoint)
	}

	return nil
}
