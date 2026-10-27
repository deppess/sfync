package transfer

import (
	"bytes"
	"errors"
	"fmt"
	"net"
	"os"
	"runtime"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/agent"
)

// sshKeySigners returns the signer(s) to authenticate with for the private
// key at keyPath.
//
// An unencrypted key is used directly. A passphrase-protected key can't be
// decrypted without prompting (sfync also runs headless as a daemon), so the
// matching key is taken from ssh-agent instead — e.g. one loaded with
// `ssh-add --apple-use-keychain` on macOS. Only the agent key matching
// keyPath's public half is offered, never the agent's whole keyring: servers
// cap authentication attempts (OpenSSH MaxAuthTries, default 6), so offering
// every agent key can exhaust the limit before the right one is tried.
//
// ag may be nil when no agent is reachable.
func sshKeySigners(keyPath string, ag agent.Agent) ([]ssh.Signer, error) {
	keyBytes, err := os.ReadFile(keyPath)
	if err != nil {
		return nil, fmt.Errorf("SSH key not found: %s", keyPath)
	}

	signer, err := ssh.ParsePrivateKey(keyBytes)
	if err == nil {
		return []ssh.Signer{signer}, nil
	}

	var missing *ssh.PassphraseMissingError
	if !errors.As(err, &missing) {
		return nil, fmt.Errorf("cannot parse SSH key %s: %w", keyPath, err)
	}

	// Encrypted key: identify it by its public half — embedded in the
	// OpenSSH key format, otherwise from the conventional .pub file.
	pub := missing.PublicKey
	if pub == nil {
		if pubBytes, rerr := os.ReadFile(keyPath + ".pub"); rerr == nil {
			pub, _, _, _, _ = ssh.ParseAuthorizedKey(pubBytes)
		}
	}
	if pub == nil {
		return nil, fmt.Errorf("SSH key %s is passphrase-protected and has no %s.pub to match against ssh-agent", keyPath, keyPath)
	}

	if ag != nil {
		agentSigners, aerr := ag.Signers()
		if aerr == nil {
			for _, s := range agentSigners {
				if bytes.Equal(s.PublicKey().Marshal(), pub.Marshal()) {
					return []ssh.Signer{s}, nil
				}
			}
		}
	}

	return nil, fmt.Errorf("SSH key %s is passphrase-protected and not loaded in ssh-agent — load it with: %s",
		keyPath, sshAddHint(keyPath))
}

// sshAddHint is the command that loads keyPath into the agent. On macOS,
// --apple-use-keychain stores the passphrase in the login keychain so the
// key is reloaded automatically, including for the background daemon.
func sshAddHint(keyPath string) string {
	if runtime.GOOS == "darwin" {
		return "ssh-add --apple-use-keychain " + keyPath
	}
	return "ssh-add " + keyPath
}

// dialAgent connects to the ssh-agent named by SSH_AUTH_SOCK. It returns a
// nil agent (and a no-op close) when no agent is configured or reachable —
// the caller only needs the agent for passphrase-protected keys, and
// sshKeySigners reports a clear error if one turns out to be required.
func dialAgent() (agent.Agent, func()) {
	sock := os.Getenv("SSH_AUTH_SOCK")
	if sock == "" {
		return nil, func() {}
	}
	conn, err := net.DialTimeout("unix", sock, DialTimeout)
	if err != nil {
		return nil, func() {}
	}
	return agent.NewClient(conn), func() { conn.Close() }
}
