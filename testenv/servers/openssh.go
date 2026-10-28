package servers

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"fmt"
	"net"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/deppess/sfync/testenv/netem"
)

// sshdPath is the system OpenSSH daemon (macOS ships OpenSSH in /usr/sbin).
const sshdPath = "/usr/sbin/sshd"

// HaveOpenSSH reports whether a system sshd is available to launch.
func HaveOpenSSH() bool {
	_, err := os.Stat(sshdPath)
	return err == nil
}

// StartOpenSSH runs the system's real OpenSSH sshd as the current user — no
// root, so it can only authenticate this user — bound to 127.0.0.1 on a
// free port, with a throwaway host key and client key, public-key auth only,
// all files in a temp dir. The SFTP subsystem is OpenSSH's internal-sftp,
// so behavior (posix-rename, limits, packet sizes) matches real servers.
//
// Skips the test if no sshd is installed.
func StartOpenSSH(tb testing.TB, p netem.Profile) *Server {
	tb.Helper()
	if !HaveOpenSSH() {
		tb.Skip("system sshd not available")
	}

	dir := tb.TempDir()
	root := filepath.Join(dir, "root")
	if err := os.Mkdir(root, 0o755); err != nil {
		tb.Fatal(err)
	}

	hostKey := filepath.Join(dir, "host_ed25519")
	clientKey := filepath.Join(dir, "client_ed25519")
	authorized := filepath.Join(dir, "authorized_keys")
	writeKeyPair(tb, hostKey, "")
	clientPub := writeKeyPair(tb, clientKey, authorized)
	_ = clientPub

	me, err := user.Current()
	if err != nil {
		tb.Fatal(err)
	}

	port := freePort(tb)
	cfgPath := filepath.Join(dir, "sshd_config")
	cfg := fmt.Sprintf(`ListenAddress 127.0.0.1:%d
HostKey %s
PidFile %s
AuthorizedKeysFile %s
AllowUsers %s
PubkeyAuthentication yes
PasswordAuthentication no
KbdInteractiveAuthentication no
UsePAM no
StrictModes no
PerSourcePenalties no
MaxStartups 64
MaxSessions 64
Subsystem sftp internal-sftp
LogLevel ERROR
`, port, hostKey, filepath.Join(dir, "sshd.pid"), authorized, me.Username)
	if err := os.WriteFile(cfgPath, []byte(cfg), 0o600); err != nil {
		tb.Fatal(err)
	}

	var stderr bytes.Buffer
	cmd := exec.Command(sshdPath, "-D", "-e", "-f", cfgPath)
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		tb.Fatalf("start sshd: %v", err)
	}
	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()
	tb.Cleanup(func() {
		cmd.Process.Kill()
		<-exited
	})

	addr := net.JoinHostPort("127.0.0.1", strconv.Itoa(port))
	if err := waitListening(addr, exited); err != nil {
		tb.Fatalf("sshd did not start: %v\n%s", err, stderr.String())
	}

	return fronted(tb, &Server{
		Kind:       "openssh",
		Root:       root,
		RemoteRoot: root,
		protocol:   "sftp",
		username:   me.Username,
		sshKey:     clientKey,
	}, addr, p)
}

// writeKeyPair writes an unencrypted ed25519 private key to path and, if
// authorizedPath is set, its public key to authorizedPath.
func writeKeyPair(tb testing.TB, path, authorizedPath string) ssh.PublicKey {
	tb.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		tb.Fatal(err)
	}
	block, err := ssh.MarshalPrivateKey(priv, "")
	if err != nil {
		tb.Fatal(err)
	}
	if err := os.WriteFile(path, pem.EncodeToMemory(block), 0o600); err != nil {
		tb.Fatal(err)
	}
	sshPub, err := ssh.NewPublicKey(pub)
	if err != nil {
		tb.Fatal(err)
	}
	if authorizedPath != "" {
		if err := os.WriteFile(authorizedPath, ssh.MarshalAuthorizedKey(sshPub), 0o600); err != nil {
			tb.Fatal(err)
		}
	}
	return sshPub
}

func freePort(tb testing.TB) int {
	tb.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		tb.Fatal(err)
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port
}

// waitListening polls until addr accepts connections, the process exits, or
// a timeout passes.
func waitListening(addr string, exited <-chan error) error {
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		select {
		case err := <-exited:
			return fmt.Errorf("process exited: %v", err)
		default:
		}
		if c, err := net.DialTimeout("tcp", addr, 200*time.Millisecond); err == nil {
			c.Close()
			return nil
		}
		time.Sleep(20 * time.Millisecond)
	}
	return fmt.Errorf("timed out waiting for %s", addr)
}

// fronted puts a shaping proxy in front of a server listening on addr (a
// pass-through proxy even for the unshaped profile, so every kind always
// pays the same proxy overhead and results stay comparable) and fills in
// the client-facing host/port.
func fronted(tb testing.TB, s *Server, addr string, p netem.Profile) *Server {
	tb.Helper()
	px, err := netem.NewProxy(addr, p)
	if err != nil {
		tb.Fatal(err)
	}
	tb.Cleanup(func() { px.Close() })
	host, portStr, _ := net.SplitHostPort(px.Addr())
	s.host = host
	s.port, _ = strconv.Atoi(portStr)
	s.Net = p
	return s
}
