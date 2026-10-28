package transfer

import (
	"fmt"
	"os"
	"sync"
	"time"

	"github.com/deppess/sfync/internal/config"
)

// freshFor is how long after a successful operation a connection is
// trusted without a liveness probe. In a burst of saves each upload would
// otherwise pay an extra round trip just to confirm what the previous one
// proved. If the connection died anyway, the operation fails, which clears
// the mark; the caller's retry then probes and reconnects.
const freshFor = 10 * time.Second

// ConnectionPool manages persistent connections for daemon mode: one
// client per profile, shared by concurrent users through leases.
//
// A client found dead (or belonging to a profile whose connection settings
// changed) is retired: no new leases are handed out for it, and it is
// closed only once its last lease is released — never underneath an
// operation still using it.
type ConnectionPool struct {
	mu          sync.Mutex
	connections map[string]*poolEntry
	profiles    map[string]*config.Profile
}

type poolEntry struct {
	client  RemoteClient
	users   int       // outstanding leases
	retired bool      // replaced or invalid: close when users reaches 0
	lastOK  time.Time // last successful operation (or liveness check)
}

// NewConnectionPool creates a new connection pool
func NewConnectionPool(profiles map[string]*config.Profile) *ConnectionPool {
	return &ConnectionPool{
		connections: make(map[string]*poolEntry),
		profiles:    profiles,
	}
}

// Acquire leases a live connection for profileName, reconnecting
// transparently if the existing one is dead. The caller must call release
// exactly once with the outcome of its work: an error makes the next
// Acquire re-check liveness.
func (p *ConnectionPool) Acquire(profileName string) (client RemoteClient, release func(error), err error) {
	p.mu.Lock()
	if e, ok := p.connections[profileName]; ok && !e.retired {
		e.users++
		fresh := time.Since(e.lastOK) < freshFor
		p.mu.Unlock()
		// Probe outside the lock, so one profile's slow network never
		// stalls lookups for another.
		if fresh || e.client.IsAlive() {
			p.mu.Lock()
			if !fresh {
				e.lastOK = time.Now()
			}
			p.mu.Unlock()
			return e.client, p.releaser(e), nil
		}
		fmt.Fprintf(os.Stderr, "Connection to %s lost, reconnecting...\n", profileName)
		p.mu.Lock()
		p.retire(profileName, e)
		p.releaseLocked(e, nil) // unlocks p.mu
		p.mu.Lock()
	}

	profile, ok := p.profiles[profileName]
	p.mu.Unlock()
	if !ok {
		return nil, nil, fmt.Errorf("unknown profile: %s", profileName)
	}

	// Dial outside the lock — Connect() can block up to DialTimeout.
	fresh := NewClient(profile)
	if err := fresh.Connect(); err != nil {
		return nil, nil, err // already wrapped by Connect()
	}

	p.mu.Lock()
	defer p.mu.Unlock()
	// Another caller may have raced us and already connected — share its
	// connection and close ours rather than leaking a socket.
	if e, ok := p.connections[profileName]; ok && !e.retired {
		e.users++
		go fresh.Close()
		return e.client, p.releaser(e), nil
	}
	e := &poolEntry{client: fresh, users: 1, lastOK: time.Now()}
	p.connections[profileName] = e
	return e.client, p.releaser(e), nil
}

func (p *ConnectionPool) releaser(e *poolEntry) func(error) {
	var once sync.Once
	return func(err error) {
		once.Do(func() {
			p.mu.Lock()
			p.releaseLocked(e, err)
		})
	}
}

// releaseLocked ends a lease; the caller holds p.mu, which this releases.
func (p *ConnectionPool) releaseLocked(e *poolEntry, err error) {
	e.users--
	if err == nil {
		e.lastOK = time.Now()
	} else {
		e.lastOK = time.Time{} // re-check liveness before trusting it again
	}
	closeNow := e.retired && e.users == 0
	p.mu.Unlock()
	if closeNow {
		e.client.Close()
	}
}

// retire stops handing out leases for e; it's closed when its last lease
// is released (or now, if it has none). Caller holds p.mu.
func (p *ConnectionPool) retire(name string, e *poolEntry) {
	if p.connections[name] == e {
		delete(p.connections, name)
	}
	if !e.retired {
		e.retired = true
		if e.users == 0 {
			go e.client.Close()
		}
	}
}

// UpdateProfiles updates the pool with new profiles from config reload.
// Connections for removed or changed profiles are retired: closed once no
// operation is using them.
func (p *ConnectionPool) UpdateProfiles(newProfiles map[string]*config.Profile) {
	p.mu.Lock()
	defer p.mu.Unlock()

	for name, oldProfile := range p.profiles {
		newProfile, exists := newProfiles[name]
		if !exists || profileChanged(oldProfile, newProfile) {
			if e, ok := p.connections[name]; ok {
				p.retire(name, e)
			}
		}
	}

	p.profiles = newProfiles
}

// CloseAll closes all connections. Used during shutdown, after users have
// stopped.
func (p *ConnectionPool) CloseAll() {
	p.mu.Lock()
	defer p.mu.Unlock()

	for _, e := range p.connections {
		e.retired = true
		e.client.Close()
	}
	p.connections = make(map[string]*poolEntry)
}

// profileChanged checks if connection-relevant fields differ — anything
// that changes where, how, or how securely sfync connects.
func profileChanged(old, newP *config.Profile) bool {
	verify := func(p *config.Profile) bool { return p.VerifyHostKey == nil || *p.VerifyHostKey }
	return old.Host != newP.Host ||
		old.Port != newP.Port ||
		old.Username != newP.Username ||
		old.Password != newP.Password ||
		old.SSHKey != newP.SSHKey ||
		old.Protocol != newP.Protocol ||
		old.FTPConnections != newP.FTPConnections ||
		old.TLSCACert != newP.TLSCACert ||
		verify(old) != verify(newP)
}
