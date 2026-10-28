package transfer

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"net"
	"os"
	"sync"
	"time"

	"github.com/jlaffaye/ftp"

	"github.com/deppess/sfync/internal/config"
)

// ftpTLSConfig builds the TLS configuration shared by every connection of
// one FTPS client.
//
// Sharing one config matters beyond tidiness: its session cache lets data
// connections resume the control connection's TLS session, which servers
// like vsftpd require by default (require_ssl_reuse=YES) — without it,
// FTPS transfers to such servers fail.
//
// With profile.TLSCACert set, the server certificate is verified against
// that CA bundle instead of the system roots (for private CAs and
// self-signed certificates); the hostname is verified either way.
func ftpTLSConfig(profile *config.Profile) (*tls.Config, error) {
	cfg := &tls.Config{
		ServerName:         profile.Host,
		MinVersion:         tls.VersionTLS12,
		ClientSessionCache: tls.NewLRUClientSessionCache(64),
	}
	if profile.TLSCACert != "" {
		pem, err := os.ReadFile(profile.TLSCACert)
		if err != nil {
			return nil, fmt.Errorf("cannot read tlsCACert: %w", err)
		}
		roots := x509.NewCertPool()
		if !roots.AppendCertsFromPEM(pem) {
			return nil, fmt.Errorf("tlsCACert %s contains no PEM certificates", profile.TLSCACert)
		}
		cfg.RootCAs = roots
	}
	return cfg, nil
}

// ftpSession is one FTP control connection plus deadline enforcement for
// it and its data connections.
//
// jlaffaye/ftp sets no deadlines on commands — a server that stops
// responding mid-session would hang a caller (and, pooled, occupy a
// connection) forever. So sfync dials every connection itself, through
// ftp.DialWithDialFunc, and applies the current operation's deadline to the
// control connection and to each data connection opened during it.
//
// Dialing ourselves has a security consequence: with a custom dial
// function, jlaffaye/ftp returns data connections as dialed, skipping the
// TLS wrapping it otherwise applies. For FTPS, dial therefore wraps every
// data connection in TLS itself — the same tls.Client(conn, config) the
// library would use — or file contents would cross the network in the
// clear. (The control connection's explicit AUTH TLS upgrade is still done
// by the library.)
type ftpSession struct {
	*ftp.ServerConn

	tlsConfig *tls.Config // nil for plain FTP

	mu       sync.Mutex
	control  net.Conn   // first connection dialed; raw TCP under any TLS
	data     []net.Conn // data connections dialed during the current operation
	deadline time.Time  // zero = none
}

// dialFTPSession opens and authenticates one control connection to addr.
// tlsConfig non-nil means FTPS (explicit AUTH TLS).
func dialFTPSession(addr, user, pass string, tlsConfig *tls.Config) (*ftpSession, error) {
	s := &ftpSession{tlsConfig: tlsConfig}
	opts := []ftp.DialOption{ftp.DialWithDialFunc(s.dial)}
	if tlsConfig != nil {
		opts = append(opts, ftp.DialWithExplicitTLS(tlsConfig))
	}

	// Bound connect + greeting + AUTH TLS + login, like every operation.
	s.begin(DialTimeout + OperationTimeout)
	conn, err := ftp.Dial(addr, opts...)
	if err != nil {
		s.closeRaw()
		return nil, err
	}
	s.ServerConn = conn
	if err := conn.Login(user, pass); err != nil {
		conn.Quit()
		return nil, err
	}
	s.end()
	return s, nil
}

// dial is jlaffaye/ftp's dial function for both the control connection
// (first call) and every data connection.
func (s *ftpSession) dial(network, address string) (net.Conn, error) {
	raw, err := net.DialTimeout(network, address, DialTimeout)
	if err != nil {
		return nil, err
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.deadline.IsZero() {
		raw.SetDeadline(s.deadline)
	}
	if s.control == nil {
		s.control = raw
		return raw, nil // the library upgrades it via AUTH TLS itself
	}

	s.data = append(s.data, raw)
	if s.tlsConfig != nil {
		// Protect the data channel (PROT P). Must be *tls.Conn: the library
		// type-asserts it to force a handshake on zero-byte uploads.
		return tls.Client(raw, s.tlsConfig), nil
	}
	return raw, nil
}

// begin starts an operation allowed to run for d: the deadline applies to
// the control connection now and to data connections as they're dialed.
// Calling it again mid-operation extends (or shortens) the budget, e.g.
// once a download's size is known.
func (s *ftpSession) begin(d time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.deadline = time.Now().Add(d)
	if s.control != nil {
		s.control.SetDeadline(s.deadline)
	}
	for _, c := range s.data {
		c.SetDeadline(s.deadline)
	}
}

// end finishes an operation: no deadline while idle in the pool, and data
// connections (closed by the library by now) are forgotten.
func (s *ftpSession) end() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.deadline = time.Time{}
	if s.control != nil {
		s.control.SetDeadline(time.Time{})
	}
	s.data = nil
}

// closeRaw closes the underlying sockets — for a dial that failed before
// there was a ServerConn to Quit.
func (s *ftpSession) closeRaw() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.control != nil {
		s.control.Close()
	}
	for _, c := range s.data {
		c.Close()
	}
}

// close ends the session: QUIT, bounded so a dead server can't hang it.
func (s *ftpSession) close() {
	if s.ServerConn == nil {
		s.closeRaw()
		return
	}
	s.begin(AliveTimeout)
	s.Quit()
	s.closeRaw()
}
