package transfer

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/agent"
)

// writeTestKey generates an ed25519 key and writes it (optionally
// passphrase-encrypted) to a temp dir, returning its path and raw key.
func writeTestKey(t *testing.T, passphrase string) (string, ed25519.PrivateKey) {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	var block *pem.Block
	if passphrase == "" {
		block, err = ssh.MarshalPrivateKey(priv, "")
	} else {
		block, err = ssh.MarshalPrivateKeyWithPassphrase(priv, "", []byte(passphrase))
	}
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "id_ed25519")
	if err := os.WriteFile(path, pem.EncodeToMemory(block), 0o600); err != nil {
		t.Fatal(err)
	}
	return path, priv
}

func samePublicKey(a, b ssh.PublicKey) bool {
	return bytes.Equal(a.Marshal(), b.Marshal())
}

func TestSSHKeySignersUnencryptedKeyIgnoresAgent(t *testing.T) {
	path, priv := writeTestKey(t, "")
	want, _ := ssh.NewSignerFromKey(priv)

	signers, err := sshKeySigners(path, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(signers) != 1 || !samePublicKey(signers[0].PublicKey(), want.PublicKey()) {
		t.Fatalf("expected exactly the file key, got %d signers", len(signers))
	}
}

func TestSSHKeySignersEncryptedKeyUsesMatchingAgentKey(t *testing.T) {
	path, priv := writeTestKey(t, "hunter2")
	want, _ := ssh.NewSignerFromKey(priv)

	// Agent holds an unrelated key plus ours — only ours may be offered.
	keyring := agent.NewKeyring()
	_, other, _ := ed25519.GenerateKey(rand.Reader)
	keyring.Add(agent.AddedKey{PrivateKey: other})
	keyring.Add(agent.AddedKey{PrivateKey: priv})

	signers, err := sshKeySigners(path, keyring)
	if err != nil {
		t.Fatal(err)
	}
	if len(signers) != 1 || !samePublicKey(signers[0].PublicKey(), want.PublicKey()) {
		t.Fatalf("expected only the matching agent key, got %d signers", len(signers))
	}
}

func TestSSHKeySignersEncryptedKeyNotInAgent(t *testing.T) {
	path, _ := writeTestKey(t, "hunter2")

	keyring := agent.NewKeyring()
	_, other, _ := ed25519.GenerateKey(rand.Reader)
	keyring.Add(agent.AddedKey{PrivateKey: other})

	for name, ag := range map[string]agent.Agent{"agent without key": keyring, "no agent": nil} {
		t.Run(name, func(t *testing.T) {
			_, err := sshKeySigners(path, ag)
			if err == nil {
				t.Fatal("expected error")
			}
			if !strings.Contains(err.Error(), "ssh-add") || !strings.Contains(err.Error(), path) {
				t.Errorf("error should tell the user how to load the key: %v", err)
			}
		})
	}
}

func TestSSHKeySignersMissingFile(t *testing.T) {
	_, err := sshKeySigners(filepath.Join(t.TempDir(), "nope"), nil)
	if err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("got %v", err)
	}
}

func TestSFTPConnectWithKeyFile(t *testing.T) {
	t.Setenv("SSH_AUTH_SOCK", "") // never touch the real agent

	path, priv := writeTestKey(t, "")
	authorized, _ := ssh.NewSignerFromKey(priv)

	profile := startSFTPServerWithKey(t, authorized.PublicKey())
	profile.Password = ""
	profile.SSHKey = path

	c := connectTestClient(t, profile)
	if _, err := c.Stat(t.TempDir()); err != nil {
		t.Fatalf("stat after key auth: %v", err)
	}
}
