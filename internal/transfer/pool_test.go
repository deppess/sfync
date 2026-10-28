package transfer

import (
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/deppess/sfync/internal/config"
)

// liveClient is a RemoteClient double counting liveness probes and closes.
type liveClient struct {
	RemoteClient // unimplemented methods panic if reached
	alive        atomic.Bool
	probes       atomic.Int32
	closed       atomic.Bool
}

func (c *liveClient) IsAlive() bool { c.probes.Add(1); return c.alive.Load() }
func (c *liveClient) Close() error  { c.closed.Store(true); return nil }

// poolWith returns a pool whose "p" profile already has client cached, as
// if dialed earlier.
func poolWith(client *liveClient, lastOK time.Time) *ConnectionPool {
	p := NewConnectionPool(map[string]*config.Profile{"p": {Host: "h", Protocol: "sftp"}})
	p.connections["p"] = &poolEntry{client: client, lastOK: lastOK}
	return p
}

func TestPoolSkipsProbeAfterRecentSuccess(t *testing.T) {
	c := &liveClient{}
	c.alive.Store(true)
	p := poolWith(c, time.Now())

	got, release, err := p.Acquire("p")
	if err != nil || got != c {
		t.Fatalf("Acquire: %v", err)
	}
	release(nil)
	if n := c.probes.Load(); n != 0 {
		t.Errorf("probed %d times right after a success, want 0", n)
	}

	// A failed operation clears the mark: the next Acquire probes.
	_, release, _ = p.Acquire("p")
	release(errors.New("connection reset"))
	_, release, _ = p.Acquire("p")
	release(nil)
	if n := c.probes.Load(); n != 1 {
		t.Errorf("probes after a failure = %d, want 1", n)
	}
}

func TestPoolProbesStaleConnection(t *testing.T) {
	c := &liveClient{}
	c.alive.Store(true)
	p := poolWith(c, time.Now().Add(-time.Hour))
	_, release, err := p.Acquire("p")
	if err != nil {
		t.Fatal(err)
	}
	release(nil)
	if c.probes.Load() != 1 {
		t.Error("stale connection handed out without a liveness probe")
	}
}

// A connection retired by a config reload while in use must not be closed
// under its user — only once the lease is released — and must not be
// handed out again.
func TestPoolRetiresWithoutClosingInUse(t *testing.T) {
	c := &liveClient{}
	c.alive.Store(true)
	p := poolWith(c, time.Now())

	_, release, _ := p.Acquire("p")
	p.UpdateProfiles(map[string]*config.Profile{"p": {Host: "other", Protocol: "sftp"}})
	if c.closed.Load() {
		t.Fatal("connection closed while a lease was outstanding")
	}
	if _, ok := p.connections["p"]; ok {
		t.Error("retired connection still offered to new users")
	}
	release(nil)
	if !c.closed.Load() {
		t.Error("retired connection not closed after its last lease")
	}
}

func TestProfileChangedIncludesSecuritySettings(t *testing.T) {
	on, off := true, false
	base := config.Profile{Host: "h", Protocol: "ftps", VerifyHostKey: &on}
	for name, mutate := range map[string]func(*config.Profile){
		"tlsCACert":      func(p *config.Profile) { p.TLSCACert = "/ca.pem" },
		"verifyHostKey":  func(p *config.Profile) { p.VerifyHostKey = &off },
		"ftpConnections": func(p *config.Profile) { p.FTPConnections = 2 },
	} {
		changed := base
		mutate(&changed)
		if !profileChanged(&base, &changed) {
			t.Errorf("changing %s doesn't trigger a reconnect", name)
		}
	}
	nilVerify := base
	nilVerify.VerifyHostKey = nil // nil means the default, true
	if profileChanged(&base, &nilVerify) {
		t.Error("verifyHostKey nil vs true treated as a change")
	}
}
