package cmd

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/deppess/sfync/internal/deps"
)

// InstallDaemon creates the systemd service file
func InstallDaemon() error {
	fmt.Println("✓ Checking dependencies...")

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

// daemonServicePath returns the systemd user service file path
func daemonServicePath() (string, error) {
	homeDir, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("cannot determine home directory: %w", err)
	}
	return filepath.Join(homeDir, ".config", "systemd", "user", "sfync.service"), nil
}

// StartDaemon starts the sfync systemd user service
func StartDaemon() error {
	svcPath, err := daemonServicePath()
	if err != nil {
		return err
	}
	if _, err := os.Stat(svcPath); errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("daemon not installed — run: sfync install-daemon")
	}

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

// StopDaemon stops the sfync systemd user service
func StopDaemon() error {
	svcPath, err := daemonServicePath()
	if err != nil {
		return err
	}
	if _, err := os.Stat(svcPath); errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("daemon not installed — run: sfync install-daemon")
	}

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

// UninstallDaemon stops, disables, and removes the systemd service
func UninstallDaemon() error {
	servicePath, err := daemonServicePath()
	if err != nil {
		return err
	}

	// Check if service exists
	if _, err := os.Stat(servicePath); errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("daemon not installed (service file not found)")
	}

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
