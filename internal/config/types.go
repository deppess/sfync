package config

import (
	"os"
	"path/filepath"
	"strings"
)

// Profile represents a single server configuration
type Profile struct {
	Host             string `json:"host"`
	Username         string `json:"username"`
	Password         string `json:"password"`
	SSHKey           string `json:"sshKey"`
	Port             int    `json:"port"`
	Protocol         string `json:"protocol"`
	RemotePath       string `json:"remotePath"`
	Context          string `json:"context"`
	AutoSync         bool   `json:"autoSync"`
	AutoSyncDebounce int    `json:"autoSyncDebounce"` // milliseconds
	// AutoSyncDeletes makes the daemon also delete the remote copy when a
	// file is deleted or renamed away locally. Opt-in: with it on, any local
	// deletion — including files vanishing during a `git checkout` — is
	// mirrored to the server.
	AutoSyncDeletes bool  `json:"autoSyncDeletes"`
	VerifyHostKey   *bool `json:"verifyHostKey"` // nil = default true (TOFU)
	// FTPConnections caps how many parallel control connections an FTP/FTPS
	// profile may open (FTP allows one command in flight per connection, so
	// this is its parallelism). Extra connections open only when work runs
	// in parallel, and sfync caps itself lower if the server refuses one.
	// 1 = a single connection. Ignored for SFTP, which multiplexes requests
	// over one connection.
	FTPConnections int `json:"ftpConnections"`
	// TLSCACert is a PEM file of CA certificates to verify an FTPS server
	// against instead of the system roots — for servers with a private CA
	// or self-signed certificate. The hostname is still verified. Ignored
	// for sftp and plain ftp.
	TLSCACert string `json:"tlsCACert"`
}

// Bounds for FTPConnections.
const (
	DefaultFTPConnections = 4
	MaxFTPConnections     = 16
)

// Config represents the entire configuration file
type Config struct {
	Profiles map[string]Profile
}

// Validate checks if all required fields are present and valid
func (p *Profile) Validate() error {
	if p.Host == "" {
		return ErrMissingHost
	}
	if p.Username == "" {
		return ErrMissingUsername
	}
	// Validate protocol
	if p.Protocol != "ftp" && p.Protocol != "ftps" && p.Protocol != "sftp" {
		return ErrInvalidProtocol
	}
	if p.FTPConnections < 0 || p.FTPConnections > MaxFTPConnections {
		return ErrInvalidFTPConnections
	}

	// Validate port
	if p.Port < 1 || p.Port > 65535 {
		return ErrInvalidPort
	}
	// For SFTP/SSH protocols, require either password or SSH key
	if p.Protocol == "sftp" {
		if p.Password == "" && p.SSHKey == "" {
			return ErrMissingPasswordOrKey
		}
	} else {
		// For FTP/FTPS, password is still required
		if p.Password == "" {
			return ErrMissingPassword
		}
	}
	// Context is optional — auto-detected from .git walk-up when not set
	return nil
}

// SetDefaults applies default values for optional fields
func (p *Profile) SetDefaults() {
	if p.Protocol == "" {
		p.Protocol = "sftp"
	}
	if p.Port == 0 {
		if p.Protocol == "sftp" {
			p.Port = 22
		} else {
			p.Port = 21
		}
	}
	if p.RemotePath == "" {
		p.RemotePath = "/"
	}
	if p.VerifyHostKey == nil {
		t := true
		p.VerifyHostKey = &t
	}
	if p.AutoSyncDebounce == 0 {
		p.AutoSyncDebounce = 2000 // 2 seconds default
	}
	if p.FTPConnections == 0 && p.Protocol != "sftp" {
		p.FTPConnections = DefaultFTPConnections
	}
	p.SSHKey = ExpandHome(p.SSHKey)
	p.Context = ExpandHome(p.Context)
	p.TLSCACert = ExpandHome(p.TLSCACert)
}

// ExpandHome expands a leading "~" or "~/" to the current user's home
// directory, the way a shell would. JSON config has no shell in between, so
// without this "~/.ssh/id_ed25519" would be read as a literal relative path.
// "~otheruser" forms are left alone. If the home directory can't be
// determined the path is returned unchanged.
func ExpandHome(p string) string {
	if p != "~" && !strings.HasPrefix(p, "~/") {
		return p
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return p
	}
	return filepath.Join(home, p[1:])
}
