package servers

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/deppess/sfync/internal/transfer"
	"github.com/deppess/sfync/testenv/netem"
)

// Every server kind must accept sfync and round-trip a file.
func TestAllServersRoundTrip(t *testing.T) {
	for name, start := range map[string]Starter{
		"openssh":     StartOpenSSH,
		"sftp-inproc": StartInProcSFTP,
		"ftp":         StartFTP,
	} {
		t.Run(name, func(t *testing.T) {
			srv := start(t, netem.Local)
			c := transfer.NewClient(srv.Profile(t.TempDir()))
			if err := c.Connect(); err != nil {
				t.Fatalf("connect: %v", err)
			}
			defer c.Close()

			src := filepath.Join(t.TempDir(), "hello.txt")
			os.WriteFile(src, []byte("hello"), 0o644)
			if err := c.Upload(src, transfer.RemotePath(srv.RemoteRoot, "hello.txt")); err != nil {
				t.Fatalf("upload: %v", err)
			}
			if got, _ := os.ReadFile(filepath.Join(srv.Root, "hello.txt")); string(got) != "hello" {
				t.Errorf("server root has %q", got)
			}
			dst := filepath.Join(t.TempDir(), "back.txt")
			if err := c.Download(transfer.RemotePath(srv.RemoteRoot, "hello.txt"), dst); err != nil {
				t.Fatalf("download: %v", err)
			}
			if got, _ := os.ReadFile(dst); string(got) != "hello" {
				t.Errorf("downloaded %q", got)
			}
		})
	}
}
