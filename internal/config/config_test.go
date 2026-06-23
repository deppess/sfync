package config

import (
	"errors"
	"testing"
)

func TestSetDefaults(t *testing.T) {
	t.Run("empty protocol defaults to sftp", func(t *testing.T) {
		p := Profile{}
		p.SetDefaults()
		if p.Protocol != "sftp" {
			t.Errorf("protocol = %q, want %q", p.Protocol, "sftp")
		}
	})

	t.Run("sftp gets port 22", func(t *testing.T) {
		p := Profile{Protocol: "sftp"}
		p.SetDefaults()
		if p.Port != 22 {
			t.Errorf("sftp port = %d, want 22", p.Port)
		}
	})

	t.Run("ftp gets port 21", func(t *testing.T) {
		p := Profile{Protocol: "ftp"}
		p.SetDefaults()
		if p.Port != 21 {
			t.Errorf("ftp port = %d, want 21", p.Port)
		}
	})

	t.Run("explicit port not overwritten", func(t *testing.T) {
		p := Profile{Protocol: "sftp", Port: 2222}
		p.SetDefaults()
		if p.Port != 2222 {
			t.Errorf("port = %d, want 2222", p.Port)
		}
	})

	t.Run("empty remotePath defaults to slash", func(t *testing.T) {
		p := Profile{}
		p.SetDefaults()
		if p.RemotePath != "/" {
			t.Errorf("remotePath = %q, want %q", p.RemotePath, "/")
		}
	})

	t.Run("verifyHostKey nil defaults to true", func(t *testing.T) {
		p := Profile{}
		p.SetDefaults()
		if p.VerifyHostKey == nil || !*p.VerifyHostKey {
			t.Error("verifyHostKey should default to true")
		}
	})

	t.Run("explicit verifyHostKey false preserved", func(t *testing.T) {
		f := false
		p := Profile{VerifyHostKey: &f}
		p.SetDefaults()
		if p.VerifyHostKey == nil || *p.VerifyHostKey {
			t.Error("explicit verifyHostKey=false should be preserved")
		}
	})

	t.Run("autoSyncDebounce defaults to 2000", func(t *testing.T) {
		p := Profile{}
		p.SetDefaults()
		if p.AutoSyncDebounce != 2000 {
			t.Errorf("autoSyncDebounce = %d, want 2000", p.AutoSyncDebounce)
		}
	})

	t.Run("explicit autoSyncDebounce preserved", func(t *testing.T) {
		p := Profile{AutoSyncDebounce: 500}
		p.SetDefaults()
		if p.AutoSyncDebounce != 500 {
			t.Errorf("autoSyncDebounce = %d, want 500", p.AutoSyncDebounce)
		}
	})
}

func TestValidate(t *testing.T) {
	validSFTP := func() Profile {
		return Profile{Host: "host", Username: "user", SSHKey: "~/.ssh/id_ed25519", Protocol: "sftp", Port: 22}
	}
	validFTP := func() Profile {
		return Profile{Host: "host", Username: "user", Password: "pass", Protocol: "ftp", Port: 21}
	}

	tests := []struct {
		name    string
		profile Profile
		wantErr error
	}{
		{"valid sftp with key", validSFTP(), nil},
		{"valid sftp with password", Profile{Host: "h", Username: "u", Password: "p", Protocol: "sftp", Port: 22}, nil},
		{"valid ftp", validFTP(), nil},
		{"missing host", func() Profile { p := validSFTP(); p.Host = ""; return p }(), ErrMissingHost},
		{"missing username", func() Profile { p := validSFTP(); p.Username = ""; return p }(), ErrMissingUsername},
		{"sftp no auth", func() Profile { p := validSFTP(); p.SSHKey = ""; return p }(), ErrMissingPasswordOrKey},
		{"ftp missing password", func() Profile { p := validFTP(); p.Password = ""; return p }(), ErrMissingPassword},
		{"invalid protocol", Profile{Host: "h", Username: "u", Password: "p", Protocol: "telnet", Port: 21}, ErrInvalidProtocol},
		{"port zero", func() Profile { p := validSFTP(); p.Port = 0; return p }(), ErrInvalidPort},
		{"port too high", func() Profile { p := validSFTP(); p.Port = 70000; return p }(), ErrInvalidPort},
		{"port 1 valid", func() Profile { p := validSFTP(); p.Port = 1; return p }(), nil},
		{"port 65535 valid", func() Profile { p := validSFTP(); p.Port = 65535; return p }(), nil},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.profile.Validate()
			if tc.wantErr == nil {
				if err != nil {
					t.Errorf("Validate() = %v, want nil", err)
				}
			} else {
				if !errors.Is(err, tc.wantErr) {
					t.Errorf("Validate() = %v, want %v", err, tc.wantErr)
				}
			}
		})
	}
}
