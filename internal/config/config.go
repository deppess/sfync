package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

var (
	ErrConfigNotFound        = errors.New("config file not found")
	ErrConfigUnreadable      = errors.New("cannot read config file")
	ErrConfigEmpty           = errors.New("config file is empty")
	ErrInvalidJSON           = errors.New("invalid JSON syntax")
	ErrMissingHost           = errors.New("missing required field: host")
	ErrMissingUsername       = errors.New("missing required field: username")
	ErrMissingPassword       = errors.New("missing required field: password")
	ErrMissingPasswordOrKey  = errors.New("missing required field: password or sshKey (at least one required for SFTP)")
	ErrInvalidProtocol       = errors.New("invalid protocol: must be 'ftp', 'ftps', or 'sftp'")
	ErrInvalidPort           = errors.New("invalid port: must be between 1 and 65535")
	ErrInvalidFTPConnections = errors.New("invalid ftpConnections: must be between 1 and 16")
	ErrProfileNotFound       = errors.New("profile not found in config")
)

const (
	DefaultConfigDir  = ".config/sfync"
	DefaultConfigFile = "config.json"
)

// GetConfigPath returns the full path to the config file
func GetConfigPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("cannot determine home directory: %w", err)
	}
	return filepath.Join(home, DefaultConfigDir, DefaultConfigFile), nil
}

// Load reads and parses the configuration file
func Load() (*Config, error) {
	configPath, err := GetConfigPath()
	if err != nil {
		return nil, err
	}

	// Check if config file exists
	info, err := os.Stat(configPath)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("%w: %s", ErrConfigNotFound, configPath)
	}
	if err == nil {
		warnIfInsecurePermissions(configPath, info)
	}

	// Read config file
	data, err := os.ReadFile(configPath)
	if err != nil {
		return nil, fmt.Errorf("%w: %s", ErrConfigUnreadable, err)
	}

	// Check if empty
	if len(data) == 0 {
		return nil, fmt.Errorf("%w: %s", ErrConfigEmpty, configPath)
	}

	// Parse JSON
	var profiles map[string]Profile
	if err := json.Unmarshal(data, &profiles); err != nil {
		return nil, fmt.Errorf("%w: %s", ErrInvalidJSON, err)
	}

	config := &Config{
		Profiles: profiles,
	}

	// Validate and set defaults for each profile
	for name, profile := range config.Profiles {
		profile.SetDefaults()
		if err := profile.Validate(); err != nil {
			return nil, fmt.Errorf("profile '%s': %w", name, err)
		}
		warnIfInsecureProfile(name, &profile)
		// Update the profile with defaults
		config.Profiles[name] = profile
	}

	return config, nil
}

// warnIfInsecureProfile warns (non-fatally) about profile settings that
// weaken transport security: plain FTP sends credentials and file contents
// unencrypted, and a disabled verifyHostKey removes protection against a
// machine-in-the-middle presenting a different host key.
func warnIfInsecureProfile(name string, p *Profile) {
	if p.Protocol == "ftp" {
		fmt.Fprintf(os.Stderr,
			"Warning: profile '%s' uses plain FTP — credentials and file contents are sent unencrypted. Prefer 'sftp' where possible.\n", name)
	}
	if p.VerifyHostKey != nil && !*p.VerifyHostKey {
		fmt.Fprintf(os.Stderr,
			"Warning: profile '%s' has verifyHostKey disabled — vulnerable to machine-in-the-middle attacks.\n", name)
	}
}

// warnIfInsecurePermissions warns (non-fatally) when the config file is
// readable or writable by anyone other than its owner. Profiles can hold
// plaintext passwords, so this is worth flagging even though the README
// already recommends `chmod 600`.
func warnIfInsecurePermissions(configPath string, info os.FileInfo) {
	if info.Mode().Perm()&0o077 != 0 {
		fmt.Fprintf(os.Stderr,
			"Warning: %s is readable by other users on this system (mode %04o).\n"+
				"  It may contain plaintext passwords — fix with: chmod 600 %s\n",
			configPath, info.Mode().Perm(), configPath)
	}
}

// GetProfile retrieves a profile by name
func (c *Config) GetProfile(name string) (*Profile, error) {
	profile, exists := c.Profiles[name]
	if !exists {
		return nil, fmt.Errorf("%w: '%s'", ErrProfileNotFound, name)
	}
	return &profile, nil
}
