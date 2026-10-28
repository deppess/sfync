package integration

import (
	"testing"

	"github.com/deppess/sfync/internal/transfer"
	"github.com/deppess/sfync/testenv/netem"
	"github.com/deppess/sfync/testenv/servers"
)

// mirrorWith runs a full mirror up with the given ftpConnections setting
// and returns the server's connection stats. Shaped as a WAN link so
// operations overlap long enough for parallelism to show.
func mirrorWith(t *testing.T, conns int, opts servers.FTPOptions) (*servers.Server, *servers.FTPStats, string) {
	t.Helper()
	srv, stats := servers.StartFTPWithStats(t, netem.Profile{Name: "test", RTT: 10e6}, opts) // 10ms
	local := localCopy(t)
	profile := srv.Profile(local)
	profile.FTPConnections = conns
	c := transfer.NewClient(profile)
	if err := c.Connect(); err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() { c.Close() })
	res, err := transfer.MirrorUp(c, profile, false)
	if err != nil || !res.Success {
		t.Fatalf("mirror up: %v %v", err, res.Errors)
	}
	return srv, stats, local
}

func TestFTPPoolRunsInParallel(t *testing.T) {
	srv, stats, local := mirrorWith(t, 4, servers.FTPOptions{})
	treesEqual(t, local, srv.Root)
	noPartials(t, srv.Root)
	if p := stats.PeakConnections(); p < 2 || p > 4 {
		t.Errorf("peak connections = %d, want 2..4 (parallel, within ftpConnections)", p)
	}
}

func TestFTPSingleConnection(t *testing.T) {
	srv, stats, local := mirrorWith(t, 1, servers.FTPOptions{})
	treesEqual(t, local, srv.Root)
	if p := stats.PeakConnections(); p != 1 {
		t.Errorf("peak connections = %d, want exactly 1", p)
	}
}

// A server allowing fewer connections than configured: sfync must stay
// within the limit after the first refusal, and the sync must still fully
// succeed on the connections it has.
func TestFTPPoolRespectsServerConnectionLimit(t *testing.T) {
	srv, stats, local := mirrorWith(t, 8, servers.FTPOptions{MaxConnections: 2})
	treesEqual(t, local, srv.Root)
	noPartials(t, srv.Root)
	if p := stats.PeakConnections(); p != 2 {
		t.Errorf("peak connections = %d, want 2 (the server's limit)", p)
	}
	if r := stats.Rejected(); r < 1 || r > 3 {
		t.Errorf("rejected = %d; want a few probes then a stable cap, not repeated attempts", r)
	}
}
