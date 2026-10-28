package integration

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/deppess/sfync/internal/transfer"
	"github.com/deppess/sfync/testenv/netem"
	"github.com/deppess/sfync/testenv/servers"
)

// Full FTPS round trip with pooled connections, asserting from the server
// side that every data connection — uploads, downloads and listings — was
// TLS-protected. A regression that sent file contents in the clear (e.g.
// a custom dial path skipping TLS on data connections) fails here.
func TestFTPSEncryptsEveryDataConnection(t *testing.T) {
	srv, stats := servers.StartFTPWithStats(t, netem.Local, servers.FTPOptions{TLS: true})
	local := localCopy(t)
	_, up := connect(t, srv, local)
	up()
	treesEqual(t, local, srv.Root)
	if second := up(); second.Uploaded != 0 {
		t.Errorf("no-op re-sync uploaded %d", second.Uploaded)
	}

	fresh := filepath.Join(t.TempDir(), "fresh")
	os.Mkdir(fresh, 0o755)
	c2, _ := connect(t, srv, fresh)
	if res, err := transfer.MirrorDown(c2, srv.Profile(fresh), false); err != nil || !res.Success {
		t.Fatalf("mirror down: %v %v", err, res.Errors)
	}
	treesEqual(t, local, fresh)

	data, tlsData := stats.DataConnections(), stats.TLSDataConnections()
	if data == 0 {
		t.Fatal("no data connections observed")
	}
	if tlsData != data {
		t.Errorf("%d of %d data connections were NOT TLS-encrypted", data-tlsData, data)
	}
}

// Certificate verification must be on: without tlsCACert, the test
// server's self-signed certificate is untrusted and connecting must fail.
func TestFTPSRejectsUntrustedCertificate(t *testing.T) {
	srv, _ := servers.StartFTPWithStats(t, netem.Local, servers.FTPOptions{TLS: true})
	profile := srv.Profile(t.TempDir())
	profile.TLSCACert = ""
	c := transfer.NewClient(profile)
	err := c.Connect()
	if err == nil {
		c.Close()
		t.Fatal("connected to a server with an untrusted certificate")
	}
	if !strings.Contains(err.Error(), "TLS") && !strings.Contains(err.Error(), "certificate") {
		t.Errorf("error doesn't mention TLS/certificate: %v", err)
	}
}
