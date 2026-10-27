package transfer

import (
	"crypto/ed25519"
	"crypto/rand"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/pkg/sftp"
	"golang.org/x/crypto/ssh"

	"github.com/deppess/sfync/internal/config"
)

// startSFTPServer runs an in-process SSH server on localhost whose "sftp"
// subsystem is pkg/sftp's server over the real filesystem, and returns a
// profile pointing at it. Host-key verification is off so the test never
// touches ~/.ssh/known_hosts.
func startSFTPServer(t *testing.T) *config.Profile {
	t.Helper()

	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := ssh.NewSignerFromKey(priv)
	if err != nil {
		t.Fatal(err)
	}
	srvConfig := &ssh.ServerConfig{
		PasswordCallback: func(c ssh.ConnMetadata, pass []byte) (*ssh.Permissions, error) {
			return nil, nil
		},
	}
	srvConfig.AddHostKey(signer)

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })

	go func() {
		for {
			nConn, err := ln.Accept()
			if err != nil {
				return
			}
			go serveSFTPConn(nConn, srvConfig)
		}
	}()

	verify := false
	host, port, _ := net.SplitHostPort(ln.Addr().String())
	portNum, _ := strconv.Atoi(port)
	return &config.Profile{
		Host: host, Port: portNum, Username: "u", Password: "p",
		Protocol: "sftp", VerifyHostKey: &verify,
	}
}

func serveSFTPConn(nConn net.Conn, cfg *ssh.ServerConfig) {
	_, chans, reqs, err := ssh.NewServerConn(nConn, cfg)
	if err != nil {
		return
	}
	go ssh.DiscardRequests(reqs)
	for newChan := range chans {
		if newChan.ChannelType() != "session" {
			newChan.Reject(ssh.UnknownChannelType, "unsupported")
			continue
		}
		ch, chReqs, err := newChan.Accept()
		if err != nil {
			continue
		}
		go func() {
			for req := range chReqs {
				ok := req.Type == "subsystem" && string(req.Payload[4:]) == "sftp"
				req.Reply(ok, nil)
				if ok {
					server, err := sftp.NewServer(ch)
					if err == nil {
						server.Serve()
					}
					ch.Close()
					return
				}
			}
		}()
	}
}

func connectTestClient(t *testing.T, profile *config.Profile) *sftpClient {
	t.Helper()
	c := newSFTPClient(profile)
	if err := c.Connect(); err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() { c.Close() })
	return c
}

func TestSFTPUploadDownloadRoundTrip(t *testing.T) {
	c := connectTestClient(t, startSFTPServer(t))
	localDir, remoteDir := t.TempDir(), t.TempDir()

	src := filepath.Join(localDir, "page.html")
	mtime := time.Date(2023, 5, 6, 7, 8, 9, 0, time.UTC)
	os.WriteFile(src, []byte("hello"), 0o640)
	os.Chtimes(src, mtime, mtime)

	remote := filepath.Join(remoteDir, "page.html")
	os.WriteFile(remote, []byte("previous live version"), 0o644)

	if err := c.Upload(src, remote); err != nil {
		t.Fatalf("upload: %v", err)
	}
	got, _ := os.ReadFile(remote)
	if string(got) != "hello" {
		t.Errorf("remote contents = %q", got)
	}
	fi, _ := os.Stat(remote)
	if !fi.ModTime().Equal(mtime) {
		t.Errorf("remote mtime = %v, want %v", fi.ModTime(), mtime)
	}
	if fi.Mode().Perm() != 0o640 {
		t.Errorf("remote mode = %v, want 0640", fi.Mode().Perm())
	}
	if names := listDir(t, remoteDir); len(names) != 1 {
		t.Errorf("partial left on remote: %v", names)
	}

	dst := filepath.Join(localDir, "downloaded.html")
	if err := c.Download(remote, dst); err != nil {
		t.Fatalf("download: %v", err)
	}
	got, _ = os.ReadFile(dst)
	if string(got) != "hello" {
		t.Errorf("downloaded contents = %q", got)
	}
}

func TestSFTPUploadResumeContinuesValidPartial(t *testing.T) {
	c := connectTestClient(t, startSFTPServer(t))
	localDir, remoteDir := t.TempDir(), t.TempDir()

	src := filepath.Join(localDir, "big.bin")
	os.WriteFile(src, []byte("0123456789"), 0o644)
	past := time.Now().Add(-time.Hour)
	os.Chtimes(src, past, past)

	// A previous attempt got the first 4 bytes across. The marker byte "X"
	// (instead of "3") proves the existing partial is appended to rather
	// than re-uploaded from scratch.
	remote := filepath.Join(remoteDir, "big.bin")
	os.WriteFile(remotePartialPath(remote), []byte("012X"), 0o644)

	if err := c.UploadResume(src, remote); err != nil {
		t.Fatalf("resume: %v", err)
	}
	got, _ := os.ReadFile(remote)
	if string(got) != "012X456789" {
		t.Errorf("remote = %q, want partial continued (012X456789)", got)
	}
}

func TestSFTPUploadResumeDiscardsStalePartial(t *testing.T) {
	c := connectTestClient(t, startSFTPServer(t))
	localDir, remoteDir := t.TempDir(), t.TempDir()

	remote := filepath.Join(remoteDir, "big.bin")
	os.WriteFile(remotePartialPath(remote), []byte("OLD!"), 0o644)
	stale := time.Now().Add(-time.Hour)
	os.Chtimes(remotePartialPath(remote), stale, stale)

	// Local file modified after the partial was written — the partial is a
	// prefix of an older version and must not be appended to.
	src := filepath.Join(localDir, "big.bin")
	os.WriteFile(src, []byte("0123456789"), 0o644)

	if err := c.UploadResume(src, remote); err != nil {
		t.Fatalf("resume: %v", err)
	}
	got, _ := os.ReadFile(remote)
	if string(got) != "0123456789" {
		t.Errorf("remote = %q, want full fresh upload", got)
	}
}

func TestSFTPFailedUploadLeavesLiveFileIntact(t *testing.T) {
	c := connectTestClient(t, startSFTPServer(t))
	remoteDir := t.TempDir()
	remote := filepath.Join(remoteDir, "live.html")
	os.WriteFile(remote, []byte("live"), 0o644)

	if err := c.Upload(filepath.Join(t.TempDir(), "missing"), remote); err == nil {
		t.Fatal("expected error uploading a missing file")
	}
	if got, _ := os.ReadFile(remote); string(got) != "live" {
		t.Errorf("live file changed: %q", got)
	}
}

func TestSFTPConcurrentDeadlinesDontInterfere(t *testing.T) {
	c := connectTestClient(t, startSFTPServer(t))
	dir := t.TempDir()

	// Many concurrent stats each register and release their own deadline;
	// afterwards no deadline may linger on the shared connection.
	done := make(chan error, 32)
	for i := 0; i < 32; i++ {
		go func() { _, err := c.Stat(dir); done <- err }()
	}
	for i := 0; i < 32; i++ {
		if err := <-done; err != nil {
			t.Fatalf("stat: %v", err)
		}
	}
	c.deadlines.mu.Lock()
	inFlight := len(c.deadlines.active)
	c.deadlines.mu.Unlock()
	if inFlight != 0 {
		t.Errorf("%d deadlines still registered after all ops finished", inFlight)
	}
}
