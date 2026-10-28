package servers

import (
	"crypto/tls"
	"errors"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
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
}

// StartFTP runs an in-process FTP server (fclairamb/ftpserverlib) serving a
// temp directory, with a throwaway user/password. Both the control
// connection and every passive data connection are shaped by p.
func StartFTP(tb testing.TB, p netem.Profile) *Server {
	return StartFTPWith(tb, p, FTPOptions{})
}

// StartFTPWith is StartFTP with explicit server capabilities.
func StartFTPWith(tb testing.TB, p netem.Profile, opts FTPOptions) *Server {
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
	drv := &ftpDriver{
		fs:   afero.NewBasePathFs(afero.NewOsFs(), root),
		user: user, pass: pass,
		net: p,
		settings: &ftpserver.Settings{
			Listener:    ln,
			PublicHost:  "127.0.0.1",
			DisableMFMT: opts.DisableMFMT,
			DisableMLSD: opts.DisableMLSD,
			DisableMLST: opts.DisableMLSD,
		},
	}
	srv := ftpserver.NewFtpServer(drv)
	srv.Logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	if err := srv.Listen(); err != nil {
		tb.Fatal(err)
	}
	go srv.Serve()
	tb.Cleanup(func() { srv.Stop() })

	return fronted(tb, &Server{
		Kind:       "ftp",
		Root:       root,
		RemoteRoot: "/",
		protocol:   "ftp",
		username:   user,
		password:   pass,
	}, ln.Addr().String(), p)
}

type ftpDriver struct {
	fs         afero.Fs
	user, pass string
	net        netem.Profile
	settings   *ftpserver.Settings
}

func (d *ftpDriver) GetSettings() (*ftpserver.Settings, error) { return d.settings, nil }
func (d *ftpDriver) ClientConnected(ftpserver.ClientContext) (string, error) {
	return "sfync test server", nil
}
func (d *ftpDriver) ClientDisconnected(ftpserver.ClientContext) {}
func (d *ftpDriver) AuthUser(_ ftpserver.ClientContext, user, pass string) (ftpserver.ClientDriver, error) {
	if user == d.user && pass == d.pass {
		return d.fs, nil
	}
	return nil, errors.New("bad credentials")
}
func (d *ftpDriver) GetTLSConfig() (*tls.Config, error) {
	return nil, errors.New("TLS not configured")
}

// WrapPassiveListener shapes passive data connections too — the client
// dials the passive port directly, bypassing the control-port proxy.
func (d *ftpDriver) WrapPassiveListener(l net.Listener) (net.Listener, error) {
	return netem.WrapListener(l, d.net)
}

var _ ftpserver.MainDriverExtensionPassiveWrapper = (*ftpDriver)(nil)
