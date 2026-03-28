package transfer

import (
	"fmt"
	"os"
	"sync"

	"github.com/deppess/sfync/internal/config"
)

// ConnectionPool manages persistent connections for daemon mode.
// Uses reconnect-on-use strategy: no keepalive goroutines,
// pings before each use, reconnects if dead.
type ConnectionPool struct {
	connections map[string]RemoteClient
	profiles    map[string]*config.Profile
	mu          sync.Mutex
}

// NewConnectionPool creates a new connection pool
func NewConnectionPool(profiles map[string]*config.Profile) *ConnectionPool {
	return &ConnectionPool{
		connections: make(map[string]RemoteClient),
		profiles:    profiles,
	}
}

// Get returns a live connection for the given profile.
// If the existing connection is dead, it reconnects transparently.
func (p *ConnectionPool) Get(profileName string) (RemoteClient, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	// Check existing connection
	if client, ok := p.connections[profileName]; ok {
		if client.IsAlive() {
			return client, nil
		}
		// Dead — close and discard
		client.Close()
		delete(p.connections, profileName)
		fmt.Fprintf(os.Stderr, "Connection to %s lost, reconnecting...\n", profileName)
	}

	// Get profile
	profile, ok := p.profiles[profileName]
	if !ok {
		return nil, fmt.Errorf("unknown profile: %s", profileName)
	}

	// New connection
	client := NewClient(profile)
	if err := client.Connect(); err != nil {
		return nil, err // already wrapped by Connect()
	}

	p.connections[profileName] = client
	return client, nil
}

// UpdateProfiles updates the pool with new profiles from config reload.
// Closes connections for removed or changed profiles.
func (p *ConnectionPool) UpdateProfiles(newProfiles map[string]*config.Profile) {
	p.mu.Lock()
	defer p.mu.Unlock()

	// Close connections for removed or changed profiles
	for name, oldProfile := range p.profiles {
		newProfile, exists := newProfiles[name]
		if !exists || profileChanged(oldProfile, newProfile) {
			if client, ok := p.connections[name]; ok {
				client.Close()
				delete(p.connections, name)
			}
		}
	}

	p.profiles = newProfiles
}

// CloseAll closes all connections. Used during shutdown.
func (p *ConnectionPool) CloseAll() {
	p.mu.Lock()
	defer p.mu.Unlock()

	for _, client := range p.connections {
		client.Close()
	}
	p.connections = make(map[string]RemoteClient)
}

// profileChanged checks if connection-relevant fields differ
func profileChanged(old, newP *config.Profile) bool {
	return old.Host != newP.Host ||
		old.Port != newP.Port ||
		old.Username != newP.Username ||
		old.Password != newP.Password ||
		old.SSHKey != newP.SSHKey ||
		old.Protocol != newP.Protocol
}
