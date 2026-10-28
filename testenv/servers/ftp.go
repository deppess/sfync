package servers

import (
	"crypto/tls"
	"errors"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"

	ftpserver "github.com/fclairamb/ftpserverlib"
	"github.com/spf13/afero"

	"github.com/deppess/sfync/testenv/netem"
)

// FTPOptions selects server capabilities, to test sfync against both
// full-featured and bare-bones FTP servers.
type FTPOptions struct {
	// DisableMFMT turns off setting file times (sfync then warns once and
	// can't preserve mtimes).
	DisableMFMT bool
	// DisableMLSD makes a LIST-only server, like vsftpd: no MLSD or MLST,
	// so listings have only minute (recent files) or day (older files)
	// timestamp precision. Both are disabled together because clients
	// (jlaffaye/ftp) decide to use MLSD from the MLST feature — a server
	// advertising MLST but refusing MLSD wouldn't model a real one.
	DisableMLSD bool
	// MaxConnections rejects clients beyond this many concurrent control
	// connections (0 = unlimited), like shared hosts' per-user limits.
	MaxConnections int
	// TLS makes it an FTPS server (explicit AUTH TLS, required) with a
	// throwaway self-signed certificate; Server.Profile then sets protocol
	// "ftps" and trusts that certificate via tlsCACert.
	TLS bool
}

// StartFTP runs an in-process FTP server (fclairamb/ftpserverlib) serving a
// temp directory, with a throwaway user/password. Both the control
// connection and every passive data connection are shaped by p.
func StartFTP(tb testing.TB, p netem.Profile) *Server {
	return StartFTPWith(tb, p, FTPOptions{})
}

// StartFTPWith is StartFTP with explicit server capabilities.
func StartFTPWith(tb testing.TB, p netem.Profile, opts FTPOptions) *Server {
	srv, _ := StartFTPWithStats(tb, p, opts)
	return srv
}

// FTPStats observes a test FTP server's connections.
type FTPStats struct {
	active, peak, rejected atomic.Int32
	rejectedIDs            sync.Map // client IDs turned away (still get a disconnect callback)
	dataConns, tlsData     atomic.Int32
}

// DataConnections is how many passive data connections clients opened.
func (s *FTPStats) DataConnections() int { return int(s.dataConns.Load()) }

// TLSDataConnections is how many of them began with a TLS handshake record
// — i.e. carried file contents encrypted.
func (s *FTPStats) TLSDataConnections() int { return int(s.tlsData.Load()) }

// PeakConnections is the most concurrent control connections seen.
func (s *FTPStats) PeakConnections() int { return int(s.peak.Load()) }

// Rejected is how many connections were refused for exceeding MaxConnections.
func (s *FTPStats) Rejected() int { return int(s.rejected.Load()) }

// StartFTPWithStats is StartFTPWith that also reports connection statistics.
func StartFTPWithStats(tb testing.TB, p netem.Profile, opts FTPOptions) (*Server, *FTPStats) {
	tb.Helper()
	root := filepath.Join(tb.TempDir(), "root")
	if err := os.Mkdir(root, 0o755); err != nil {
		tb.Fatal(err)
	}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		tb.Fatal(err)
	}

	const user, pass = "sfync", "sfync-test"
	stats := &FTPStats{}
	drv := &ftpDriver{
		stats:    stats,
		maxConns: opts.MaxConnections,
		fs:       afero.NewBasePathFs(afero.NewOsFs(), root),
		user:     user, pass: pass,
		net: p,
		settings: &ftpserver.Settings{
			Listener:    ln,
			PublicHost:  "127.0.0.1",
			DisableMFMT: opts.DisableMFMT,
			DisableMLSD: opts.DisableMLSD,
			DisableMLST: opts.DisableMLSD,
		},
	}
	var caPath string
	if opts.TLS {
		cert, pemPath := selfSignedCert(tb)
		drv.tlsConfig = &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12}
		drv.settings.TLSRequired = ftpserver.MandatoryEncryption
		caPath = pemPath
	}

	srv := ftpserver.NewFtpServer(drv)
	srv.Logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	if err := srv.Listen(); err != nil {
		tb.Fatal(err)
	}
	go srv.Serve()
	tb.Cleanup(func() { srv.Stop() })

	protocol := "ftp"
	if opts.TLS {
		protocol = "ftps"
	}
	return fronted(tb, &Server{
		Kind:       protocol,
		Root:       root,
		RemoteRoot: "/",
		protocol:   protocol,
		username:   user,
		password:   pass,
		tlsCACert:  caPath,
	}, ln.Addr().String(), p), stats
}

type ftpDriver struct {
	tlsConfig  *tls.Config // nil = no TLS
	stats      *FTPStats
	maxConns   int
	fs         afero.Fs
	user, pass string
	net        netem.Profile
	settings   *ftpserver.Settings
}

func (d *ftpDriver) GetSettings() (*ftpserver.Settings, error) { return d.settings, nil }
func (d *ftpDriver) ClientConnected(cc ftpserver.ClientContext) (string, error) {
	n := d.stats.active.Add(1)
	if d.maxConns > 0 && int(n) > d.maxConns {
		d.stats.active.Add(-1)
		d.stats.rejected.Add(1)
		d.stats.rejectedIDs.Store(cc.ID(), true)
		return "too many connections", errors.New("connection limit reached")
	}
	for {
		peak := d.stats.peak.Load()
		if n <= peak || d.stats.peak.CompareAndSwap(peak, n) {
			break
		}
	}
	return "sfync test server", nil
}

func (d *ftpDriver) ClientDisconnected(cc ftpserver.ClientContext) {
	if _, rejected := d.stats.rejectedIDs.LoadAndDelete(cc.ID()); rejected {
		return
	}
	d.stats.active.Add(-1)
}
func (d *ftpDriver) AuthUser(_ ftpserver.ClientContext, user, pass string) (ftpserver.ClientDriver, error) {
	if user == d.user && pass == d.pass {
		return d.fs, nil
	}
	return nil, errors.New("bad credentials")
}
func (d *ftpDriver) GetTLSConfig() (*tls.Config, error) {
	if d.tlsConfig == nil {
		return nil, errors.New("TLS not configured")
	}
	return d.tlsConfig, nil
}

// WrapPassiveListener shapes passive data connections too — the client
// dials the passive port directly, bypassing the control-port proxy.
//
// The result is also wrapped in a sniffing listener that classifies each
// data connection by its first byte, before any TLS layer the server adds.
func (d *ftpDriver) WrapPassiveListener(l net.Listener) (net.Listener, error) {
	shaped, err := netem.WrapListener(l, d.net)
	if err != nil {
		return nil, err
	}
	return &sniffListener{Listener: shaped, stats: d.stats}, nil
}

// sniffListener counts data connections and how many start with a TLS
// handshake record (content type 0x16). For an upload the client's first
// byte is either a TLS ClientHello or file data; for a download a
// plaintext client sends nothing at all — so "began with 0x16" is exactly
// "the client protected this data connection with TLS".
type sniffListener struct {
	net.Listener
	stats *FTPStats
}

func (l *sniffListener) Accept() (net.Conn, error) {
	c, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}
	l.stats.dataConns.Add(1)
	return &sniffConn{Conn: c, stats: l.stats}, nil
}

type sniffConn struct {
	net.Conn
	stats *FTPStats
	seen  atomic.Bool
}

func (c *sniffConn) Read(b []byte) (int, error) {
	n, err := c.Conn.Read(b)
	if n > 0 && c.seen.CompareAndSwap(false, true) && b[0] == 0x16 {
		c.stats.tlsData.Add(1)
	}
	return n, err
}

var _ ftpserver.MainDriverExtensionPassiveWrapper = (*ftpDriver)(nil)
