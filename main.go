package main

import (
	"fmt"
	"os"
	"runtime"

	"github.com/deppess/sfync/cmd"
)

const version = "3.3.0"

// requireUnix gates the subcommands that need Linux- or macOS-specific
// background-service/mount tooling (rclone mounting, systemd/launchd).
// Sync commands (push/pull/up/down/diff/current) are plain SFTP/FTP and
// work on any OS, so they're not gated here.
func requireUnix(feature, reason string) {
	if runtime.GOOS != "linux" && runtime.GOOS != "darwin" {
		fmt.Fprintf(os.Stderr, "Error: `sfync %s` is not supported on this OS\n", feature)
		fmt.Fprintf(os.Stderr, "Current OS: %s\n", runtime.GOOS)
		fmt.Fprintf(os.Stderr, "\nThis command requires: %s\n", reason)
		os.Exit(1)
	}
}

// exitOnError ends a failed command: the error goes to stderr — even when
// a desktop notification also reported it, since the terminal may be the
// only place the user is looking — and the exit status is 1.
func exitOnError(err error) {
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
}

func main() {
	if len(os.Args) < 2 {
		printUsage()
		os.Exit(1)
	}

	command := os.Args[1]

	switch command {
	case "up":
		if len(os.Args) < 3 {
			fmt.Println("Usage: sfync up <profile> [file]")
			os.Exit(1)
		}
		// Optional file argument for editor integration
		var contextFile string
		if len(os.Args) >= 4 {
			contextFile = os.Args[3]
		}
		exitOnError(cmd.Up(os.Args[2], contextFile))

	case "down":
		if len(os.Args) < 3 {
			fmt.Println("Usage: sfync down <profile> [file]")
			os.Exit(1)
		}
		// Optional file argument for editor integration
		var contextFile string
		if len(os.Args) >= 4 {
			contextFile = os.Args[3]
		}
		exitOnError(cmd.Down(os.Args[2], contextFile))

	case "diff":
		// Usage: sfync diff <up|down> <profile> [file]
		if len(os.Args) < 4 {
			fmt.Println("Usage: sfync diff <up|down> <profile> [file]")
			fmt.Println("  sfync diff up <profile>    Show what would be uploaded (dry-run)")
			fmt.Println("  sfync diff down <profile>  Show what would be downloaded (dry-run)")
			os.Exit(1)
		}
		direction := os.Args[2]
		profileName := os.Args[3]
		var contextFile string
		if len(os.Args) >= 5 {
			contextFile = os.Args[4]
		}
		exitOnError(cmd.Diff(direction, profileName, contextFile))

	case "push":
		if len(os.Args) < 4 {
			fmt.Println("Usage: sfync push <profile> <file>")
			os.Exit(1)
		}
		exitOnError(cmd.Push(os.Args[2], os.Args[3]))

	case "pull":
		if len(os.Args) < 4 {
			fmt.Println("Usage: sfync pull <profile> <file>")
			os.Exit(1)
		}
		exitOnError(cmd.Pull(os.Args[2], os.Args[3]))

	case "current":
		if len(os.Args) < 4 {
			fmt.Println("Usage: sfync current <profile> <file>")
			os.Exit(1)
		}
		exitOnError(cmd.Current(os.Args[2], os.Args[3]))

	case "mount":
		requireUnix("mount", "rclone (and sshfs on Linux)")
		if len(os.Args) < 3 {
			fmt.Println("Usage: sfync mount <profile> [--yazi]")
			os.Exit(1)
		}
		profileName := os.Args[2]
		openYazi := false
		if len(os.Args) >= 4 && os.Args[3] == "--yazi" {
			openYazi = true
		}
		exitOnError(cmd.Mount(profileName, openYazi))

	case "unmount":
		requireUnix("unmount", "rclone (and sshfs on Linux)")
		if len(os.Args) < 3 {
			fmt.Println("Usage: sfync unmount <profile|--all>")
			os.Exit(1)
		}
		if os.Args[2] == "--all" {
			exitOnError(cmd.Unmount("", true))
		} else {
			exitOnError(cmd.Unmount(os.Args[2], false))
		}

	case "mounts":
		requireUnix("mounts", "rclone (and sshfs on Linux)")
		exitOnError(cmd.Mounts())

	case "daemon":
		requireUnix("daemon", "a desktop notifier (notify-send on Linux, osascript on macOS)")
		exitOnError(cmd.Daemon())

	case "install-daemon":
		requireUnix("install-daemon", "a background service manager (systemd on Linux, launchd on macOS)")
		exitOnError(cmd.InstallDaemon())

	case "uninstall-daemon":
		requireUnix("uninstall-daemon", "a background service manager (systemd on Linux, launchd on macOS)")
		exitOnError(cmd.UninstallDaemon())

	case "on":
		requireUnix("on", "a background service manager (systemd on Linux, launchd on macOS)")
		exitOnError(cmd.StartDaemon())

	case "off":
		requireUnix("off", "a background service manager (systemd on Linux, launchd on macOS)")
		exitOnError(cmd.StopDaemon())

	case "version", "--version", "-v":
		fmt.Printf("sfync version %s\n", version)

	case "--completions":
		if len(os.Args) < 3 {
			fmt.Fprintln(os.Stderr, "Usage: sfync --completions <fish|bash|zsh>")
			os.Exit(1)
		}
		exitOnError(cmd.Completions(os.Args[2]))

	case "--list-profiles":
		exitOnError(cmd.ListProfiles())

	case "help", "--help", "-h":
		printUsage()

	default:
		fmt.Printf("Unknown command: %s\n\n", command)
		printUsage()
		os.Exit(1)
	}
}

func printUsage() {
	fmt.Println(`sfync - SFTP/FTP synchronization and mounting tool

USAGE:
  sfync <command> <profile> [options]

SYNC COMMANDS:
  up <profile>              Upload local directory to remote (full sync)
  down <profile>            Download remote directory to local (full sync)
  diff up <profile>         Show what would be uploaded (dry-run)
  diff down <profile>       Show what would be downloaded (dry-run)
  push <profile> <file>     Upload a single file
  pull <profile> <file>     Download a single file
  current <profile> <file>  Upload current file (editor integration)

MOUNT COMMANDS:
  mount <profile>           Mount remote filesystem
  mount <profile> --yazi    Mount and open in yazi file manager
  unmount <profile>         Unmount a profile's filesystem
  unmount --all             Unmount all mounted filesystems
  mounts                    List currently mounted profiles

DAEMON COMMANDS:
  daemon                    Run auto-sync daemon (watches for file changes)
  install-daemon            Install background service for auto-sync
  uninstall-daemon          Remove background service
  on                        Start the auto-sync daemon service
  off                       Stop the auto-sync daemon service

OTHER:
  version                   Show version information
  help                      Show this help message

CONFIGURATION:
  Config file: ~/.config/sfync/config.json

  Example config:
  {
    "myserver": {
      "host": "sftp.example.com",
      "username": "user",
      "sshKey": "~/.ssh/id_ed25519",
      "port": 22,
      "protocol": "sftp",
      "remotePath": "/var/www/html",
      "context": "/home/user/projects/website",
      "autoSync": true,
      "autoSyncDebounce": 2000
    }
  }

EXAMPLES:
  sfync up myserver
  sfync mount myserver --yazi
  sfync push myserver index.html
  sfync unmount --all`)
}
