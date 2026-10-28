package servers

import (
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"net"
	"os"
	"path/filepath"
	"testing"

	"github.com/pkg/sftp"
	"golang.org/x/crypto/ssh"

	"github.com/deppess/sfync/testenv/netem"
)

// StartInProcSFTP runs pkg/sftp's server over an in-process SSH server
// (password auth, throwaway host key) serving a temp directory through the
// real filesystem. Faster to start than OpenSSH and dependency-free, but
// it's not OpenSSH: use StartOpenSSH for realistic numbers.
func StartInProcSFTP(tb testing.TB, p netem.Profile) *Server {
	tb.Helper()
	root := filepath.Join(tb.TempDir(), "root")
	if err := os.Mkdir(root, 0o755); err != nil {
		tb.Fatal(err)
	}

	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		tb.Fatal(err)
	}
	signer, err := ssh.NewSignerFromKey(priv)
	if err != nil {
		tb.Fatal(err)
	}
	const user, pass = "sfync", "sfync-test"
	cfg := &ssh.ServerConfig{
		PasswordCallback: func(c ssh.ConnMetadata, pw []byte) (*ssh.Permissions, error) {
			if c.User() == user && string(pw) == pass {
				return nil, nil
			}
			return nil, errors.New("denied")
		},
	}
	cfg.AddHostKey(signer)

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		tb.Fatal(err)
	}
	tb.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go serveSFTP(c, cfg)
		}
	}()

	return fronted(tb, &Server{
		Kind:       "sftp-inproc",
		Root:       root,
		RemoteRoot: root,
		protocol:   "sftp",
		username:   user,
		password:   pass,
	}, ln.Addr().String(), p)
}

func serveSFTP(c net.Conn, cfg *ssh.ServerConfig) {
	defer c.Close()
	_, chans, reqs, err := ssh.NewServerConn(c, cfg)
	if err != nil {
		return
	}
	go ssh.DiscardRequests(reqs)
	for nc := range chans {
		if nc.ChannelType() != "session" {
			nc.Reject(ssh.UnknownChannelType, "unsupported")
			continue
		}
		ch, chReqs, err := nc.Accept()
		if err != nil {
			continue
		}
		go func() {
			defer ch.Close()
			for req := range chReqs {
				ok := req.Type == "subsystem" && len(req.Payload) > 4 && string(req.Payload[4:]) == "sftp"
				req.Reply(ok, nil)
				if ok {
					if srv, err := sftp.NewServer(ch); err == nil {
						srv.Serve()
					}
					return
				}
			}
		}()
	}
}
