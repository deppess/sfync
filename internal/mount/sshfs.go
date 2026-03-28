package mount

import (
	"bytes"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"sync"

	"github.com/deppess/sfync/internal/config"
)

// mountSSHFS mounts using sshfs
func mountSSHFS(profile *config.Profile, mountPoint string) error {
	// Build remote path
	remote := fmt.Sprintf("%s@%s:%s", profile.Username, profile.Host, profile.RemotePath)

	// Determine authentication method
	useSSHKey := profile.SSHKey != ""

	// StrictHostKeyChecking=no / UserKnownHostsFile=/dev/null skips host key
	// verification for the FUSE mount. The SFTP sync path uses its own TOFU
	// logic; for a mount we trade that check for simplicity.
	args := []string{
		remote,
		mountPoint,
		"-p", fmt.Sprintf("%d", profile.Port),
		"-o", "StrictHostKeyChecking=no",
		"-o", "UserKnownHostsFile=/dev/null",
		"-o", "reconnect",
		"-o", "ServerAliveInterval=15",
		"-o", "ServerAliveCountMax=3",
	}

	// Add authentication-specific options
	if useSSHKey {
		// Use SSH key authentication
		args = append(args, "-o", fmt.Sprintf("IdentityFile=%s", profile.SSHKey))
	} else {
		// Use password authentication
		args = append(args, "-o", "password_stdin")
	}

	cmd := exec.Command("sshfs", args...)

	// Capture stderr for error messages
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return fmt.Errorf("failed to create stderr pipe: %w", err)
	}

	// Handle password authentication
	if !useSSHKey {
		// Create pipe for password
		stdin, err := cmd.StdinPipe()
		if err != nil {
			return fmt.Errorf("failed to create stdin pipe: %w", err)
		}

		// Start the command
		if err := cmd.Start(); err != nil {
			return fmt.Errorf("failed to start sshfs: %w", err)
		}

		// Write password to stdin
		if _, err := io.WriteString(stdin, profile.Password+"\n"); err != nil {
			return fmt.Errorf("failed to write password: %w", err)
		}
		stdin.Close()
	} else {
		// Start the command (no password needed)
		if err := cmd.Start(); err != nil {
			return fmt.Errorf("failed to start sshfs: %w", err)
		}
	}

	// Drain stderr concurrently into a buffer so the pipe is fully read
	// before cmd.Wait() closes it — avoids the race where reading after
	// Wait() returns an empty result.
	var stderrBuf bytes.Buffer
	var stderrWg sync.WaitGroup
	stderrWg.Add(1)
	go func() {
		defer stderrWg.Done()
		io.Copy(&stderrBuf, stderr) //nolint:errcheck // best-effort drain
	}()

	// Wait for command to complete
	if err := cmd.Wait(); err != nil {
		// Ensure stderr goroutine has finished before reading the buffer
		stderrWg.Wait()
		errMsg := strings.TrimSpace(stderrBuf.String())

		// Parse common errors
		if strings.Contains(errMsg, "Connection refused") {
			return fmt.Errorf("connection refused to %s:%d", profile.Host, profile.Port)
		}
		if strings.Contains(errMsg, "Permission denied") || strings.Contains(errMsg, "Authentication failed") {
			return fmt.Errorf("authentication failed for %s@%s", profile.Username, profile.Host)
		}
		if strings.Contains(errMsg, "No such file or directory") {
			return fmt.Errorf("remote path not found: %s", profile.RemotePath)
		}
		if strings.Contains(errMsg, "Name or service not known") {
			return fmt.Errorf("host not found: %s", profile.Host)
		}

		if errMsg != "" {
			return fmt.Errorf("sshfs error: %s", errMsg)
		}
		return fmt.Errorf("sshfs failed: %w", err)
	}

	return nil
}
