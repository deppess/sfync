package mount

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"

	"golang.org/x/crypto/ssh"

	"github.com/deppess/sfync/internal/config"
)

// buildRcloneRemoteConfig generates the [sfync] remote stanza for the
// profile's protocol. sftp mirrors the key-first/password-fallback
// precedence used by internal/transfer/sftp.go; ftp/ftps are password-only,
// as ftp always has been — ftps additionally sets explicit_tls so rclone
// upgrades the control connection to TLS (RFC 4217) before login, matching
// internal/transfer/ftp.go's Connect().
func buildRcloneRemoteConfig(profile *config.Profile) (string, error) {
	if profile.Protocol == "sftp" {
		var authLine string
		if profile.SSHKey != "" {
			authLine = fmt.Sprintf("key_file = %s\n", profile.SSHKey)
			// Passphrase-protected key: have rclone take the matching key
			// from ssh-agent (it selects it via key_file's .pub), the same
			// way sync does in internal/transfer/sshauth.go.
			if keyIsEncrypted(profile.SSHKey) {
				authLine += "key_use_agent = true\n"
			}
		} else {
			obscuredPass, err := obscurePassword(profile.Password)
			if err != nil {
				return "", fmt.Errorf("failed to obscure password: %w", err)
			}
			authLine = fmt.Sprintf("pass = %s\n", obscuredPass)
		}
		// rclone skips host key validation entirely unless given a
		// known_hosts file. Point it at the same file sync uses, so
		// verifyHostKey protects mounts too. (Mount pre-records unknown
		// hosts via TOFU first — see preflightHostKey — since rclone itself
		// rejects hosts not already in the file.)
		var hostKeyLine string
		if profile.VerifyHostKey == nil || *profile.VerifyHostKey {
			knownHosts, err := knownHostsPath()
			if err != nil {
				return "", err
			}
			hostKeyLine = fmt.Sprintf("known_hosts_file = %s\n", knownHosts)
		}
		return fmt.Sprintf("[sfync]\ntype = sftp\nhost = %s\nuser = %s\nport = %d\n%s%s",
			profile.Host, profile.Username, profile.Port, authLine, hostKeyLine), nil
	}

	obscuredPass, err := obscurePassword(profile.Password)
	if err != nil {
		return "", fmt.Errorf("failed to obscure password: %w", err)
	}
	var tlsLine string
	if profile.Protocol == "ftps" {
		tlsLine = "explicit_tls = true\n"
	}
	return fmt.Sprintf("[sfync]\ntype = ftp\nhost = %s\nuser = %s\npass = %s\nport = %d\n%s",
		profile.Host, profile.Username, obscuredPass, profile.Port, tlsLine), nil
}

// knownHostsPath is the OpenSSH known_hosts file, shared with sync's TOFU
// verification (internal/transfer/hostkey.go).
func knownHostsPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("cannot determine home directory: %w", err)
	}
	return filepath.Join(home, ".ssh", "known_hosts"), nil
}

// keyIsEncrypted reports whether the private key at path is
// passphrase-protected. Unreadable or unparseable keys report false and are
// left for rclone to report.
func keyIsEncrypted(path string) bool {
	data, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	_, err = ssh.ParsePrivateKey(data)
	var missing *ssh.PassphraseMissingError
	return errors.As(err, &missing)
}

// mountRclone mounts using rclone. On Linux this uses the FUSE-backed
// `rclone mount`. macOS Homebrew builds of rclone don't ship FUSE support,
// so darwin uses `rclone nfsmount` instead (a local NFS server rclone runs
// itself — no macFUSE kernel extension required). Both subcommands accept
// the same flags and are torn down the same way (plain `umount`).
func mountRclone(profile *config.Profile, mountPoint string) error {
	remoteConfig, err := buildRcloneRemoteConfig(profile)
	if err != nil {
		return err
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

	_, writeErr := tmpCfg.WriteString(remoteConfig)
	tmpCfg.Close()
	if writeErr != nil {
		return fmt.Errorf("failed to write temp config: %w", writeErr)
	}

	mountSubcommand := "mount"
	if runtime.GOOS == "darwin" {
		mountSubcommand = "nfsmount"
	}

	args := []string{
		mountSubcommand,
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
		if strings.Contains(errMsg, "Login incorrect") || strings.Contains(errMsg, "530") ||
			strings.Contains(errMsg, "unable to authenticate") || strings.Contains(errMsg, "Permission denied") {
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
