package mount

import (
	"fmt"
	"net"
	"runtime"
	"strconv"
	"time"

	"github.com/deppess/sfync/internal/config"
	"github.com/deppess/sfync/internal/transfer"
)

// IsReachable checks if the remote server is accessible via TCP.
func IsReachable(profile *config.Profile) error {
	addr := net.JoinHostPort(profile.Host, strconv.Itoa(profile.Port))
	conn, err := net.DialTimeout("tcp", addr, 5*time.Second)
	if err != nil {
		return fmt.Errorf("cannot reach %s: %w", addr, err)
	}
	conn.Close()
	return nil
}

// preflightHostKey runs sync's own SSH handshake before an rclone SFTP mount
// when host key verification is on. rclone validates against known_hosts
// but has no trust-on-first-use: it rejects any host not already listed.
// Connecting through sfync first applies the same TOFU policy as sync —
// records a never-seen host, hard-rejects a changed key — so a first mount
// works and a tampered host fails with sync's clear error rather than
// rclone's. It also surfaces auth problems before rclone daemonizes.
//
// Not needed for Linux sshfs, which applies TOFU itself via
// StrictHostKeyChecking=accept-new.
func preflightHostKey(profile *config.Profile) error {
	if profile.Protocol != "sftp" || !UsesRclone(profile) {
		return nil
	}
	if profile.VerifyHostKey != nil && !*profile.VerifyHostKey {
		return nil
	}
	client := transfer.NewClient(profile)
	if err := client.Connect(); err != nil {
		return err
	}
	return client.Close()
}

// UsesRclone reports whether profile mounts via rclone rather than sshfs.
// macOS Homebrew's rclone has no FUSE support, so darwin always mounts via
// rclone nfsmount regardless of protocol; Linux keeps sftp on sshfs, ftp on
// rclone FUSE.
func UsesRclone(profile *config.Profile) bool {
	return runtime.GOOS == "darwin" || profile.Protocol != "sftp"
}
