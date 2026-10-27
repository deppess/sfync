package mount

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/crypto/ssh"

	"github.com/deppess/sfync/internal/config"
)

// These tests only inspect the generated rclone config — they never run
// rclone, mount anything, or touch the real home directory.

func writeKey(t *testing.T, passphrase string) string {
	t.Helper()
	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	var block *pem.Block
	var err error
	if passphrase == "" {
		block, err = ssh.MarshalPrivateKey(priv, "")
	} else {
		block, err = ssh.MarshalPrivateKeyWithPassphrase(priv, "", []byte(passphrase))
	}
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "id_ed25519")
	os.WriteFile(path, pem.EncodeToMemory(block), 0o600)
	return path
}

func sftpProfile(keyPath string, verify *bool) *config.Profile {
	return &config.Profile{
		Host: "example.com", Username: "u", Port: 22, Protocol: "sftp",
		SSHKey: keyPath, VerifyHostKey: verify,
	}
}

func TestRcloneConfigHostKeyVerification(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	key := writeKey(t, "")
	wantLine := "known_hosts_file = " + filepath.Join(home, ".ssh", "known_hosts") + "\n"

	on, off := true, false
	for name, tc := range map[string]struct {
		verify *bool
		want   bool
	}{
		"default (nil) verifies": {nil, true},
		"explicit true verifies": {&on, true},
		"explicit false skips":   {&off, false},
	} {
		t.Run(name, func(t *testing.T) {
			cfg, err := buildRcloneRemoteConfig(sftpProfile(key, tc.verify))
			if err != nil {
				t.Fatal(err)
			}
			if got := strings.Contains(cfg, wantLine); got != tc.want {
				t.Errorf("known_hosts_file present = %v, want %v\n%s", got, tc.want, cfg)
			}
		})
	}
}

func TestRcloneConfigEncryptedKeyUsesAgent(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	plain, err := buildRcloneRemoteConfig(sftpProfile(writeKey(t, ""), nil))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(plain, "key_use_agent") {
		t.Errorf("unencrypted key should not force agent:\n%s", plain)
	}

	enc, err := buildRcloneRemoteConfig(sftpProfile(writeKey(t, "hunter2"), nil))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(enc, "key_use_agent = true\n") {
		t.Errorf("encrypted key should set key_use_agent:\n%s", enc)
	}
}

func TestRcloneConfigFTPHasNoSSHOptions(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	cfg, err := buildRcloneRemoteConfig(&config.Profile{
		Host: "h", Username: "u", Password: "p", Port: 21, Protocol: "ftps",
	})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(cfg, "known_hosts") || strings.Contains(cfg, "key_") {
		t.Errorf("ftp config has ssh options:\n%s", cfg)
	}
	if !strings.Contains(cfg, "explicit_tls = true") {
		t.Errorf("ftps config missing explicit_tls:\n%s", cfg)
	}
}

func TestPreflightHostKeySkipsWithoutNetwork(t *testing.T) {
	off := false
	// Neither case may dial: host is unroutable, so any connect would fail.
	cases := map[string]*config.Profile{
		"ftp":             {Host: "192.0.2.1", Port: 21, Protocol: "ftp", Username: "u", Password: "p"},
		"verify disabled": {Host: "192.0.2.1", Port: 22, Protocol: "sftp", Username: "u", Password: "p", VerifyHostKey: &off},
	}
	for name, p := range cases {
		t.Run(name, func(t *testing.T) {
			if err := preflightHostKey(p); err != nil {
				t.Errorf("preflight should be a no-op, got %v", err)
			}
		})
	}
}
