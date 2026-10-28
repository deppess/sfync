// Package servers starts throwaway SFTP and FTP servers for sfync's
// integration tests and benchmarks.
//
// Every server listens on 127.0.0.1 only, serves a temp directory, uses
// throwaway credentials, and is stopped by tb.Cleanup. Nothing touches the
// user's ~/.ssh, sfync config, ssh-agent, or any external network. Each
// server is fronted by a netem shaping layer so tests can emulate latency
// and bandwidth.
package servers

import (
	"testing"

	"github.com/deppess/sfync/internal/config"
	"github.com/deppess/sfync/testenv/netem"
)

// Server is a running test server.
type Server struct {
	// Kind identifies the implementation, e.g. "openssh", "sftp-inproc", "ftp".
	Kind string
	// Root is the local directory backing the server's remote root, for
	// inspecting results directly.
	Root string
	// RemoteRoot is the path the server exposes Root as — what to put in a
	// profile's remotePath.
	RemoteRoot string
	// Net is the emulated network profile between client and server.
	Net netem.Profile

	host      string
	port      int
	protocol  string
	username  string
	password  string
	sshKey    string
	tlsCACert string
}

// Profile returns an sfync profile for this server, syncing context
// (a local directory) with the server's remote root. Host key verification
// is disabled so tests never read or write ~/.ssh/known_hosts.
func (s *Server) Profile(context string) *config.Profile {
	verify := false
	p := &config.Profile{
		Host:          s.host,
		Port:          s.port,
		Protocol:      s.protocol,
		Username:      s.username,
		Password:      s.password,
		SSHKey:        s.sshKey,
		RemotePath:    s.RemoteRoot,
		Context:       context,
		VerifyHostKey: &verify,
		TLSCACert:     s.tlsCACert,
	}
	p.SetDefaults()
	return p
}

// Starter launches a server kind under a network profile.
type Starter func(tb testing.TB, net netem.Profile) *Server
