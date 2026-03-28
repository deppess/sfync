package transfer

import (
	"errors"
	"fmt"
	"io/fs"
	"net"
	"os"
	"path/filepath"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"
)

// tofuCallback returns an ssh.HostKeyCallback implementing Trust-On-First-Use.
//
// If verifyHostKey is false, returns InsecureIgnoreHostKey (accepts any key).
// If true: known+match=accept, known+mismatch=hard reject, unknown=accept+save.
func tofuCallback(verifyHostKey bool) (ssh.HostKeyCallback, error) {
	if !verifyHostKey {
		return ssh.InsecureIgnoreHostKey(), nil
	}

	// Locate known_hosts
	homeDir, err := os.UserHomeDir()
	if err != nil {
		return nil, fmt.Errorf("cannot determine home directory: %w", err)
	}

	sshDir := filepath.Join(homeDir, ".ssh")
	knownHostsPath := filepath.Join(sshDir, "known_hosts")

	// Ensure ~/.ssh/ exists
	if err := os.MkdirAll(sshDir, 0700); err != nil {
		return nil, fmt.Errorf("cannot create ~/.ssh directory: %w", err)
	}

	// Ensure known_hosts file exists
	if _, err := os.Stat(knownHostsPath); errors.Is(err, fs.ErrNotExist) {
		f, createErr := os.OpenFile(knownHostsPath, os.O_CREATE|os.O_WRONLY, 0644)
		if createErr != nil {
			return nil, fmt.Errorf("cannot create known_hosts file: %w", createErr)
		}
		f.Close()
	}

	// Load known_hosts
	hostKeyCallback, err := knownhosts.New(knownHostsPath)
	if err != nil {
		return nil, fmt.Errorf("cannot read known_hosts: %w", err)
	}

	// Wrap callback with TOFU logic
	return func(hostname string, remote net.Addr, key ssh.PublicKey) error {
		err := hostKeyCallback(hostname, remote, key)
		if err == nil {
			// Case 1: Host known, key matches — accept
			return nil
		}

		var keyErr *knownhosts.KeyError
		if errors.As(err, &keyErr) {
			if len(keyErr.Want) > 0 {
				// Host is known — check if this is a genuine key change
				// or just a different key type (e.g., ecdsa vs ed25519)
				presentedType := key.Type()
				sameTypeMismatch := false
				for _, wantKey := range keyErr.Want {
					if wantKey.Key.Type() == presentedType {
						sameTypeMismatch = true
						break
					}
				}

				if sameTypeMismatch {
					// Case 2a: Same key type, different key — hard reject
					return fmt.Errorf(
						"host key mismatch for %s — possible security issue. "+
							"Remove old key from ~/.ssh/known_hosts to continue",
						hostname,
					)
				}

				// Case 2b: Different key type — host known, new algorithm
				// Safe to accept and add alongside existing entries
				fmt.Fprintf(os.Stderr, "Notice: Adding %s key for %s to known hosts\n", presentedType, hostname)
				if writeErr := appendKnownHost(knownHostsPath, hostname, key); writeErr != nil {
					fmt.Fprintf(os.Stderr, "Warning: Could not save host key: %v\n", writeErr)
				}
				return nil
			}

			// Case 3: Host unknown — accept and save
			fmt.Fprintf(os.Stderr, "Notice: Adding %s to known hosts\n", hostname)
			if writeErr := appendKnownHost(knownHostsPath, hostname, key); writeErr != nil {
				fmt.Fprintf(os.Stderr, "Warning: Could not save host key: %v\n", writeErr)
				// Don't fail the connection over a write error
			}
			return nil
		}

		// Some other error from knownhosts
		return err
	}, nil
}

// appendKnownHost appends a host key entry to the known_hosts file.
// Uses O_APPEND which is atomic for small writes on Linux.
func appendKnownHost(khPath string, hostname string, key ssh.PublicKey) error {
	f, err := os.OpenFile(khPath, os.O_APPEND|os.O_WRONLY|os.O_CREATE, 0644)
	if err != nil {
		return err
	}
	defer f.Close()

	// Use knownhosts.Normalize to match the format the callback expects
	line := knownhosts.Line([]string{knownhosts.Normalize(hostname)}, key)
	_, err = fmt.Fprintln(f, line)
	return err
}
