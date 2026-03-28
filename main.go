package main

import (
	"fmt"
	"os"
	"runtime"

	"github.com/deppess/sfync/cmd"
)

const version = "3.0.0"

func main() {
	// Linux-only check
	if runtime.GOOS != "linux" {
		fmt.Fprintf(os.Stderr, "Error: sfync is Linux-only\n")
		fmt.Fprintf(os.Stderr, "Current OS: %s\n", runtime.GOOS)
		fmt.Fprintf(os.Stderr, "\nThis tool requires Linux-specific features:\n")
		fmt.Fprintf(os.Stderr, "  - FUSE filesystem support (sshfs/rclone)\n")
		fmt.Fprintf(os.Stderr, "  - notify-send (libnotify)\n")
		os.Exit(1)
	}

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
		if err := cmd.Up(os.Args[2], contextFile); err != nil {
			os.Exit(1)
		}

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
		if err := cmd.Down(os.Args[2], contextFile); err != nil {
			os.Exit(1)
		}

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
		if err := cmd.Diff(direction, profileName, contextFile); err != nil {
			os.Exit(1)
		}

	case "push":
		if len(os.Args) < 4 {
			fmt.Println("Usage: sfync push <profile> <file>")
			os.Exit(1)
		}
		if err := cmd.Push(os.Args[2], os.Args[3]); err != nil {
			os.Exit(1)
		}

	case "pull":
		if len(os.Args) < 4 {
			fmt.Println("Usage: sfync pull <profile> <file>")
			os.Exit(1)
		}
		if err := cmd.Pull(os.Args[2], os.Args[3]); err != nil {
			os.Exit(1)
		}

	case "current":
		if len(os.Args) < 4 {
			fmt.Println("Usage: sfync current <profile> <file>")
			os.Exit(1)
		}
		if err := cmd.Current(os.Args[2], os.Args[3]); err != nil {
			os.Exit(1)
		}

	case "mount":
		if len(os.Args) < 3 {
			fmt.Println("Usage: sfync mount <profile> [--yazi]")
			os.Exit(1)
		}
		profileName := os.Args[2]
		openYazi := false
		if len(os.Args) >= 4 && os.Args[3] == "--yazi" {
			openYazi = true
		}
		if err := cmd.Mount(profileName, openYazi); err != nil {
			os.Exit(1)
		}

	case "unmount":
		if len(os.Args) < 3 {
			fmt.Println("Usage: sfync unmount <profile|--all>")
			os.Exit(1)
		}
		if os.Args[2] == "--all" {
			if err := cmd.Unmount("", true); err != nil {
				os.Exit(1)
			}
		} else {
			if err := cmd.Unmount(os.Args[2], false); err != nil {
				os.Exit(1)
			}
		}

	case "mounts":
		if err := cmd.Mounts(); err != nil {
			os.Exit(1)
		}

	case "daemon":
		if err := cmd.Daemon(); err != nil {
			fmt.Fprintf(os.Stderr, "Error: %v\n", err)
			os.Exit(1)
		}

	case "install-daemon":
		if err := cmd.InstallDaemon(); err != nil {
			fmt.Fprintf(os.Stderr, "Error: %v\n", err)
			os.Exit(1)
		}

	case "uninstall-daemon":
		if err := cmd.UninstallDaemon(); err != nil {
			fmt.Fprintf(os.Stderr, "Error: %v\n", err)
			os.Exit(1)
		}

	case "on":
		if err := cmd.StartDaemon(); err != nil {
			fmt.Fprintf(os.Stderr, "Error: %v\n", err)
			os.Exit(1)
		}

	case "off":
		if err := cmd.StopDaemon(); err != nil {
			fmt.Fprintf(os.Stderr, "Error: %v\n", err)
			os.Exit(1)
		}

	case "version", "--version", "-v":
		fmt.Printf("sfync version %s\n", version)

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
  install-daemon            Install systemd user service for auto-sync
  uninstall-daemon          Remove systemd user service
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
