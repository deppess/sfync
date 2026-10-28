package bench

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/deppess/sfync/internal/transfer"
	"github.com/deppess/sfync/testenv/netem"
)

// BenchmarkRemoteOps times single remote operations on an unshaped link,
// separating server-side cost from round trips: Stat (one request, no
// listing), and ReadDir on directories of several sizes.
func BenchmarkRemoteOps(b *testing.B) {
	for _, srvName := range selection("SFYNC_BENCH_SERVERS", "openssh,ftp") {
		b.Run("server="+srvName, func(b *testing.B) {
			srv := allServers[srvName](b, netem.Local)
			c := connect(b, srv, b.TempDir())

			for _, n := range []int{0, 20, 200} {
				dir := filepath.Join(srv.Root, fmt.Sprintf("dir%d", n))
				os.Mkdir(dir, 0o755)
				for i := range n {
					os.WriteFile(filepath.Join(dir, fmt.Sprintf("f%03d", i)), []byte("x"), 0o644)
				}
			}
			b.Run("op=stat", func(b *testing.B) {
				for b.Loop() {
					if _, err := c.Stat(transfer.RemotePath(srv.RemoteRoot, "dir0")); err != nil {
						b.Fatal(err)
					}
				}
			})
			for _, n := range []int{0, 20, 200} {
				b.Run(fmt.Sprintf("op=readdir/entries=%d", n), func(b *testing.B) {
					p := transfer.RemotePath(srv.RemoteRoot, fmt.Sprintf("dir%d", n))
					for b.Loop() {
						if _, err := c.ReadDir(p); err != nil {
							b.Fatal(err)
						}
					}
				})
			}
		})
	}
}
