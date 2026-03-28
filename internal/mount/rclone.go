package mount

import (
	"fmt"
	"os/exec"
	"strings"

	"github.com/deppess/sfync/internal/config"
)

// mountRclone mounts using rclone
func mountRclone(profile *config.Profile, mountPoint string) error {
	// obscurePassword passes the plaintext password as a CLI arg to rclone,
	// which means it's briefly visible in `ps` output — unavoidable with rclone's
	// inline config model. Use an rclone config file for stricter environments.
	obscuredPass, err := obscurePassword(profile.Password)
	if err != nil {
		return fmt.Errorf("failed to obscure password: %w", err)
	}

	// ":ftp:<path>" tells rclone to mount from that subdirectory on the server,
	// so we respect profile.RemotePath instead of always mounting from FTP root.
	remote := ":ftp:" + profile.RemotePath

	args := []string{
		"mount",
		remote,
		mountPoint,
		"--ftp-host", profile.Host,
		"--ftp-user", profile.Username,
		"--ftp-pass", obscuredPass,
		"--ftp-port", fmt.Sprintf("%d", profile.Port),
		"--vfs-cache-mode", "writes",
		"--daemon",
		"--no-checksum",
		"--no-modtime",
	}

	cmd := exec.Command("rclone", args...)
	output, err := cmd.CombinedOutput()
	if err != nil {
		errMsg := strings.TrimSpace(string(output))

		// Parse common errors
		if strings.Contains(errMsg, "connection refused") {
			return fmt.Errorf("connection refused to %s:%d", profile.Host, profile.Port)
		}
		if strings.Contains(errMsg, "Login incorrect") || strings.Contains(errMsg, "530") {
			return fmt.Errorf("authentication failed for %s@%s", profile.Username, profile.Host)
		}
		if strings.Contains(errMsg, "No such file") {
			return fmt.Errorf("remote path not found: %s", profile.RemotePath)
		}

		if errMsg != "" {
			return fmt.Errorf("rclone error: %s", errMsg)
		}
		return fmt.Errorf("rclone mount failed: %w", err)
	}

	return nil
}

// obscurePassword uses rclone's obscure function to encode the password
func obscurePassword(password string) (string, error) {
	cmd := exec.Command("rclone", "obscure", password)
	output, err := cmd.Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(output)), nil
}
