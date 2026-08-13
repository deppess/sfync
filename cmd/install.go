package cmd

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/deppess/sfync/internal/deps"
	"github.com/deppess/sfync/internal/notify"
)

// darwinServiceLabel is the launchd Label for the macOS LaunchAgent,
// matching the module's reverse-domain form (github.com/deppess/sfync).
const darwinServiceLabel = "com.deppess.sfync"

// InstallDaemon creates the background-service definition: a systemd user
// unit on Linux, a launchd LaunchAgent on macOS.
func InstallDaemon() error {
	fmt.Println("✓ Checking dependencies...")

	if runtime.GOOS == "darwin" {
		return installDaemonDarwin()
	}
	return installDaemonLinux()
}

func installDaemonLinux() error {
	// Check systemd
	if !deps.Check("systemctl") {
		return fmt.Errorf("systemd not found — daemon mode requires systemd")
	}
	fmt.Println("  ✓ systemd found")

	// Check notify-send
	if !deps.Check("notify-send") {
		return fmt.Errorf("notify-send not installed. Install with: sudo pacman -S libnotify")
	}
	fmt.Println("  ✓ notify-send found")

	// Find sfync binary location
	binaryPath, err := exec.LookPath("sfync")
	if err != nil {
		return fmt.Errorf("cannot find sfync in PATH: %w", err)
	}

	// Get absolute path
	absBinaryPath, err := filepath.Abs(binaryPath)
	if err != nil {
		return fmt.Errorf("cannot resolve sfync path: %w", err)
	}

	// Create systemd user directory
	homeDir, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("cannot determine home directory: %w", err)
	}

	systemdUserDir := filepath.Join(homeDir, ".config", "systemd", "user")
	if err := os.MkdirAll(systemdUserDir, 0755); err != nil {
		return fmt.Errorf("cannot create systemd user directory: %w", err)
	}

	// Service file template
	serviceContent := fmt.Sprintf(`[Unit]
Description=sfync Auto-Sync Daemon
Documentation=https://github.com/deppess/sfync
After=network-online.target

[Service]
Type=simple
ExecStart=%s daemon
Restart=on-failure
RestartSec=10s

StandardOutput=journal
StandardError=journal
SyslogIdentifier=sfync

[Install]
WantedBy=default.target
`, absBinaryPath)

	// Write service file
	servicePath := filepath.Join(systemdUserDir, "sfync.service")
	if err := os.WriteFile(servicePath, []byte(serviceContent), 0644); err != nil {
		return fmt.Errorf("failed to write service file: %w", err)
	}

	// Reload systemd daemon
	cmd := exec.Command("systemctl", "--user", "daemon-reload")
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("failed to reload systemd daemon: %w", err)
	}

	fmt.Printf("✓ Created systemd service: %s\n", servicePath)
	fmt.Println("\nTo start the daemon:")
	fmt.Println("  sfync on")
	fmt.Println("\nTo enable auto-start on login:")
	fmt.Println("  systemctl --user enable sfync")
	fmt.Println("\nTo view logs:")
	fmt.Println("  journalctl --user -u sfync -f")

	return nil
}

// installDaemonDarwin writes a launchd LaunchAgent plist. Unlike a systemd
// unit file, anything in ~/Library/LaunchAgents is auto-loaded (and, because
// this plist sets KeepAlive, auto-started) by launchd at every login — so
// there's no separate "enable auto-start" step to document here.
func installDaemonDarwin() error {
	fmt.Println("  Installing sfync-notifier.app...")
	if err := notify.Reinstall(); err != nil {
		return fmt.Errorf("failed to install notifier helper: %w", err)
	}
	fmt.Println("  ✓ sfync-notifier.app installed to ~/Library/Application Support/sfync.noindex")

	binaryPath, err := exec.LookPath("sfync")
	if err != nil {
		return fmt.Errorf("cannot find sfync in PATH: %w", err)
	}
	absBinaryPath, err := filepath.Abs(binaryPath)
	if err != nil {
		return fmt.Errorf("cannot resolve sfync path: %w", err)
	}

	homeDir, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("cannot determine home directory: %w", err)
	}

	agentsDir := filepath.Join(homeDir, "Library", "LaunchAgents")
	if err := os.MkdirAll(agentsDir, 0755); err != nil {
		return fmt.Errorf("cannot create LaunchAgents directory: %w", err)
	}

	logsDir := filepath.Join(homeDir, "Library", "Logs")
	if err := os.MkdirAll(logsDir, 0755); err != nil {
		return fmt.Errorf("cannot create Logs directory: %w", err)
	}
	logPath := filepath.Join(logsDir, "sfync.log")
	errLogPath := filepath.Join(logsDir, "sfync.error.log")

	plistContent := fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>Label</key>
	<string>%s</string>
	<key>ProgramArguments</key>
	<array>
		<string>%s</string>
		<string>daemon</string>
	</array>
	<key>KeepAlive</key>
	<dict>
		<key>SuccessfulExit</key>
		<false/>
	</dict>
	<key>ThrottleInterval</key>
	<integer>10</integer>
	<key>StandardOutPath</key>
	<string>%s</string>
	<key>StandardErrorPath</key>
	<string>%s</string>
</dict>
</plist>
`, darwinServiceLabel, absBinaryPath, logPath, errLogPath)

	plistPath, err := daemonServicePathDarwin()
	if err != nil {
		return err
	}
	if err := os.WriteFile(plistPath, []byte(plistContent), 0644); err != nil {
		return fmt.Errorf("failed to write plist file: %w", err)
	}

	fmt.Printf("✓ Created launchd agent: %s\n", plistPath)

	fmt.Println("\nRequesting notification permission — click Allow if macOS prompts you...")
	if err := notify.TestNotification(); err != nil {
		fmt.Fprintf(os.Stderr, "⚠ Test notification failed: %v\n", err)
		fmt.Fprintln(os.Stderr, "  You can grant access later in System Settings > Notifications > sfync")
	} else {
		fmt.Println("✓ Notifications enabled")
	}

	fmt.Println("\nTo start the daemon:")
	fmt.Println("  sfync on")
	fmt.Println("\n(it will also start automatically at your next login)")
	fmt.Println("\nTo view logs:")
	fmt.Printf("  tail -f %s\n", logPath)

	return nil
}

// daemonServicePath returns the OS-specific service definition path:
// the systemd user unit on Linux, the launchd plist on macOS.
func daemonServicePath() (string, error) {
	if runtime.GOOS == "darwin" {
		return daemonServicePathDarwin()
	}
	homeDir, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("cannot determine home directory: %w", err)
	}
	return filepath.Join(homeDir, ".config", "systemd", "user", "sfync.service"), nil
}

func daemonServicePathDarwin() (string, error) {
	homeDir, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("cannot determine home directory: %w", err)
	}
	return filepath.Join(homeDir, "Library", "LaunchAgents", darwinServiceLabel+".plist"), nil
}

// darwinDomainTarget and darwinServiceTarget are launchctl domain/service
// specifiers for the per-user GUI domain (required since notifications go
// out via osascript, which needs a GUI session).
func darwinDomainTarget() string {
	return fmt.Sprintf("gui/%d", os.Getuid())
}

func darwinServiceTarget() string {
	return fmt.Sprintf("gui/%d/%s", os.Getuid(), darwinServiceLabel)
}

// StartDaemon starts the sfync background service.
func StartDaemon() error {
	svcPath, err := daemonServicePath()
	if err != nil {
		return err
	}
	if _, err := os.Stat(svcPath); errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("daemon not installed — run: sfync install-daemon")
	}

	if runtime.GOOS == "darwin" {
		return startDaemonDarwin(svcPath)
	}
	return startDaemonLinux()
}

func startDaemonLinux() error {
	if err := exec.Command("systemctl", "--user", "start", "sfync").Run(); err != nil {
		return fmt.Errorf("failed to start daemon: %w", err)
	}

	out, _ := exec.Command("systemctl", "--user", "is-active", "sfync").Output()
	state := strings.TrimSpace(string(out))
	if state == "active" {
		fmt.Println("✓ Daemon is running")
	} else {
		fmt.Fprintf(os.Stderr, "✗ Daemon failed to start (state: %s)\n", state)
		fmt.Fprintf(os.Stderr, "  Check logs: journalctl --user -u sfync -n 20\n")
		return fmt.Errorf("daemon did not reach active state")
	}
	return nil
}

// startDaemonDarwin starts the LaunchAgent. `bootstrap` both loads and
// starts it (the plist's KeepAlive means launchd starts it as soon as it's
// loaded, regardless of RunAtLoad). If it's already loaded — e.g. it came
// up automatically at login — bootstrap fails, so fall back to `kickstart
// -k` to force a (re)start.
func startDaemonDarwin(plistPath string) error {
	target := darwinServiceTarget()

	if err := exec.Command("launchctl", "bootstrap", darwinDomainTarget(), plistPath).Run(); err != nil {
		if err := exec.Command("launchctl", "kickstart", "-k", target).Run(); err != nil {
			return fmt.Errorf("failed to start daemon: %w", err)
		}
	}

	// Give launchd a moment to fork and register the process before checking.
	time.Sleep(300 * time.Millisecond)

	out, _ := exec.Command("launchctl", "print", target).Output()
	if strings.Contains(string(out), "state = running") {
		fmt.Println("✓ Daemon is running")
	} else {
		fmt.Fprintf(os.Stderr, "✗ Daemon failed to start\n")
		fmt.Fprintf(os.Stderr, "  Check logs: tail -f ~/Library/Logs/sfync.error.log\n")
		return fmt.Errorf("daemon did not reach running state")
	}
	return nil
}

// StopDaemon stops the sfync background service.
func StopDaemon() error {
	svcPath, err := daemonServicePath()
	if err != nil {
		return err
	}
	if _, err := os.Stat(svcPath); errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("daemon not installed — run: sfync install-daemon")
	}

	if runtime.GOOS == "darwin" {
		return stopDaemonDarwin()
	}
	return stopDaemonLinux()
}

func stopDaemonLinux() error {
	if err := exec.Command("systemctl", "--user", "stop", "sfync").Run(); err != nil {
		return fmt.Errorf("failed to stop daemon: %w", err)
	}

	// inactive or failed both mean it is no longer running — treat as success
	out, _ := exec.Command("systemctl", "--user", "is-active", "sfync").Output()
	state := strings.TrimSpace(string(out))
	if state == "inactive" || state == "failed" {
		fmt.Println("✓ Daemon stopped")
	} else {
		fmt.Fprintf(os.Stderr, "⚠ Unexpected state after stop: %s\n", state)
	}
	return nil
}

// stopDaemonDarwin unloads the agent with `bootout`. A plain `kill` won't
// do — this plist's KeepAlive respawns a killed-but-still-loaded job almost
// immediately, disabled bit or not (verified empirically). `bootout` fully
// removes it from the domain, which is the only thing that reliably keeps
// it down. It comes back on `sfync on`, or automatically at next login.
func stopDaemonDarwin() error {
	target := darwinServiceTarget()
	output, err := exec.Command("launchctl", "bootout", target).CombinedOutput()
	if err != nil && !strings.Contains(string(output), "No such process") {
		return fmt.Errorf("failed to stop daemon: %s", strings.TrimSpace(string(output)))
	}
	fmt.Println("✓ Daemon stopped")
	return nil
}

// UninstallDaemon stops, disables, and removes the background service.
func UninstallDaemon() error {
	servicePath, err := daemonServicePath()
	if err != nil {
		return err
	}

	// Check if service exists
	if _, err := os.Stat(servicePath); errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("daemon not installed (service file not found)")
	}

	if runtime.GOOS == "darwin" {
		return uninstallDaemonDarwin(servicePath)
	}
	return uninstallDaemonLinux(servicePath)
}

func uninstallDaemonLinux(servicePath string) error {
	// Stop the service
	cmd := exec.Command("systemctl", "--user", "stop", "sfync")
	if err := cmd.Run(); err != nil {
		fmt.Fprintf(os.Stderr, "Warning: Failed to stop daemon: %v\n", err)
	} else {
		fmt.Println("✓ Stopped daemon")
	}

	// Disable the service
	cmd = exec.Command("systemctl", "--user", "disable", "sfync")
	if err := cmd.Run(); err != nil {
		fmt.Fprintf(os.Stderr, "Warning: Failed to disable daemon: %v\n", err)
	} else {
		fmt.Println("✓ Disabled auto-start")
	}

	// Remove service file
	if err := os.Remove(servicePath); err != nil {
		return fmt.Errorf("failed to remove service file: %w", err)
	}
	fmt.Println("✓ Removed service file")

	// Reload systemd daemon
	cmd = exec.Command("systemctl", "--user", "daemon-reload")
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("failed to reload systemd daemon: %w", err)
	}

	fmt.Println("\n✓ Daemon uninstalled")
	return nil
}

func uninstallDaemonDarwin(plistPath string) error {
	target := darwinServiceTarget()
	output, err := exec.Command("launchctl", "bootout", target).CombinedOutput()
	if err != nil && !strings.Contains(string(output), "No such process") {
		fmt.Fprintf(os.Stderr, "Warning: Failed to stop daemon: %s\n", strings.TrimSpace(string(output)))
	} else {
		fmt.Println("✓ Stopped daemon")
	}

	if err := os.Remove(plistPath); err != nil {
		return fmt.Errorf("failed to remove plist file: %w", err)
	}
	fmt.Println("✓ Removed plist file")

	fmt.Println("\n✓ Daemon uninstalled")
	return nil
}
