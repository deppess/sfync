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
	ErrConfigNotFound       = errors.New("config file not found")
	ErrConfigUnreadable     = errors.New("cannot read config file")
	ErrConfigEmpty          = errors.New("config file is empty")
	ErrInvalidJSON          = errors.New("invalid JSON syntax")
	ErrMissingHost          = errors.New("missing required field: host")
	ErrMissingUsername      = errors.New("missing required field: username")
	ErrMissingPassword      = errors.New("missing required field: password")
	ErrMissingPasswordOrKey = errors.New("missing required field: password or sshKey (at least one required for SFTP)")
	ErrInvalidProtocol      = errors.New("invalid protocol: must be 'ftp' or 'sftp'")
	ErrInvalidPort          = errors.New("invalid port: must be between 1 and 65535")
	ErrProfileNotFound      = errors.New("profile not found in config")
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
	if _, err := os.Stat(configPath); errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("%w: %s", ErrConfigNotFound, configPath)
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
		// Update the profile with defaults
		config.Profiles[name] = profile
	}

	return config, nil
}

// GetProfile retrieves a profile by name
func (c *Config) GetProfile(name string) (*Profile, error) {
	profile, exists := c.Profiles[name]
	if !exists {
		return nil, fmt.Errorf("%w: '%s'", ErrProfileNotFound, name)
	}
	return &profile, nil
}
