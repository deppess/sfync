package mount

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"

	"github.com/deppess/sfync/internal/config"
)

// mountRclone mounts using rclone.
func mountRclone(profile *config.Profile, mountPoint string) error {
	obscuredPass, err := obscurePassword(profile.Password)
	if err != nil {
		return fmt.Errorf("failed to obscure password: %w", err)
	}

	// Write a named-remote config with mode 0600 so credentials are not visible
	// in the rclone mount process's command line (visible to all users via ps).
	// Rclone reads the config before forking the daemon, so the file can be
	// safely removed after cmd.Wait() returns.
	tmpCfg, err := os.CreateTemp("", "sfync-rclone-*.conf")
	if err != nil {
		return fmt.Errorf("failed to create temp config: %w", err)
	}
	tmpName := tmpCfg.Name()
	defer os.Remove(tmpName)

	if err := os.Chmod(tmpName, 0600); err != nil {
		tmpCfg.Close()
		return fmt.Errorf("failed to secure temp config: %w", err)
	}

	_, writeErr := fmt.Fprintf(tmpCfg, "[sfync]\ntype = ftp\nhost = %s\nuser = %s\npass = %s\nport = %d\n",
		profile.Host, profile.Username, obscuredPass, profile.Port)
	tmpCfg.Close()
	if writeErr != nil {
		return fmt.Errorf("failed to write temp config: %w", writeErr)
	}

	args := []string{
		"mount",
		"sfync:" + profile.RemotePath,
		mountPoint,
		"--config", tmpName,
		"--vfs-cache-mode", "writes",
		"--daemon",
		"--no-checksum",
		"--no-modtime",
	}

	cmd := exec.Command("rclone", args...)
	output, err := cmd.CombinedOutput()
	if err != nil {
		errMsg := strings.TrimSpace(string(output))

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

// obscurePassword encodes a password using rclone's obfuscation scheme
// (AES-256-CTR with a random IV, base64-URL encoded) so it can be written
// to an rclone config file. Key is rclone's public constant from
// fs/config/obscure/obscure.go — intentionally "obfuscation not encryption."
func obscurePassword(password string) (string, error) {
	cryptKey := []byte{
		0x9c, 0x93, 0x5b, 0x48, 0x73, 0x0a, 0x55, 0x4d,
		0x6b, 0xfd, 0x7c, 0x63, 0xc8, 0x86, 0xa9, 0x2b,
		0xd3, 0x90, 0x19, 0x8e, 0xb8, 0x12, 0x8a, 0xfb,
		0xf4, 0xde, 0x16, 0x2b, 0x8b, 0x95, 0xf6, 0x38,
	}
	plaintext := []byte(password)
	buf := make([]byte, aes.BlockSize+len(plaintext))
	iv := buf[:aes.BlockSize]
	if _, err := io.ReadFull(rand.Reader, iv); err != nil {
		return "", fmt.Errorf("obscure: generate iv: %w", err)
	}
	block, err := aes.NewCipher(cryptKey)
	if err != nil {
		return "", fmt.Errorf("obscure: create cipher: %w", err)
	}
	cipher.NewCTR(block, iv).XORKeyStream(buf[aes.BlockSize:], plaintext)
	return base64.RawURLEncoding.EncodeToString(buf), nil
}
